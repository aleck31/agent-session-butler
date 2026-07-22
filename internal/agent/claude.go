package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeCodeAgent: Claude Code stores sessions as
// ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl. The real cwd lives inside the
// file, so we read it from the contents rather than decoding the directory name
// (which is lossy when the path contains '-', and encoded differently per OS).
type ClaudeCodeAgent struct{}

func (ClaudeCodeAgent) Name() string { return "Claude Code" }

func claudeProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func (ClaudeCodeAgent) Installed() bool {
	root := claudeProjectsRoot()
	if root == "" {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

func (a ClaudeCodeAgent) Scan() []Session {
	root := claudeProjectsRoot()
	projectDirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Session
	for _, d := range projectDirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		// Only .jsonl files directly under the project dir (skip memory/ etc.).
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}
			if s, ok := a.parse(filepath.Join(dir, f.Name())); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// parse does a fast scan: read only the file's head to get cwd/sessionId (they
// appear within the first few lines). Title and message count are left
// nil/placeholder and filled in lazily — the full file (often tens of MB) is
// never parsed here.
func (a ClaudeCodeAgent) parse(path string) (Session, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return Session{}, false
	}
	fileSize := info.Size()
	modified := info.ModTime()

	var cwd, sessionID string
	sawAnyLine := false

	forEachLine(path, func(line string) bool {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			return true // skip unparsable line, keep going
		}
		sawAnyLine = true
		if cwd == "" {
			if c, ok := obj["cwd"].(string); ok {
				cwd = c
			}
		}
		if sessionID == "" {
			if s, ok := obj["sessionId"].(string); ok {
				sessionID = s
			}
		}
		// Stop as soon as we have both — no need to read the rest.
		return cwd == "" || sessionID == ""
	})
	if !sawAnyLine {
		return Session{}, false
	}

	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	id := sessionID
	if id == "" {
		id = stem
	}
	if cwd == "" {
		cwd = "(unknown)"
	}
	return Session{
		ID:           id,
		Agent:        a.Name(),
		Cwd:          cwd,
		Title:        stem, // placeholder; real title resolved lazily
		MessageCount: nil,
		FileSize:     fileSize,
		ModifiedAt:   modified,
		Locked:       false,
		CacheKey:     path,
		FilePaths:    []string{path},
	}, true
}

// primaryFile returns the single .jsonl this session is stored in.
func (ClaudeCodeAgent) primaryFile(s Session) string {
	if len(s.FilePaths) > 0 {
		return s.FilePaths[0]
	}
	return ""
}

// Enrich does one pass over the file: tally user/assistant messages and resolve
// the title (ai-title, else first user message). Fills both lazy fields.
func (a ClaudeCodeAgent) Enrich(s Session) Session {
	count := 0
	var aiTitle, firstUserText string
	forEachLine(a.primaryFile(s), func(line string) bool {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			return true
		}
		switch obj["type"] {
		case "ai-title":
			if t, ok := obj["aiTitle"].(string); ok {
				aiTitle = t
			}
		case "user":
			count++
			if firstUserText == "" {
				firstUserText = extractText(obj["message"])
			}
		case "assistant":
			count++
		}
		return true
	})

	resolved := aiTitle
	if resolved == "" {
		t := strings.TrimSpace(firstUserText)
		if len([]rune(t)) > 60 {
			t = string([]rune(t)[:60])
		}
		resolved = t
	}
	if resolved == "" {
		resolved = strings.TrimSuffix(filepath.Base(a.primaryFile(s)), ".jsonl")
	}
	if resolved == "" {
		resolved = "(untitled)"
	}

	s.MessageCount = &count
	s.Title = resolved
	return s
}

// Delete removes the session's .jsonl file.
func (ClaudeCodeAgent) Delete(s Session) error { return deleteFiles(s) }

// extractText pulls plain text from a `message` field (content may be a string
// or an array of parts).
func extractText(message any) string {
	msg, ok := message.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := msg["content"].(string); ok {
		return s
	}
	if arr, ok := msg["content"].([]any); ok {
		for _, p := range arr {
			part, ok := p.(map[string]any)
			if ok && part["type"] == "text" {
				if t, ok := part["text"].(string); ok {
					return t
				}
			}
		}
	}
	return ""
}
