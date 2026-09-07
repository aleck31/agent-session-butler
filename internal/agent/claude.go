package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
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
	sawConversation := false // any user/assistant line — i.e. a real chat

	forEachLine(path, func(line string) bool {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			return true // skip unparsable line, keep going
		}
		sawAnyLine = true
		if t, _ := obj["type"].(string); t == "user" || t == "assistant" {
			sawConversation = true
		}
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
		// Keep reading until we've confirmed a real conversation AND have both
		// cwd and sessionId. cwd only appears on user/assistant rows, so a file
		// that never yields cwd is scanned in full and revealed as metadata-only.
		return !sawConversation || cwd == "" || sessionID == ""
	})
	if !sawAnyLine {
		return Session{}, false
	}
	// Skip metadata-only files (only ai-title / agent-name rows, no chat): they
	// have no conversation, no cwd, and 0 messages — not real sessions.
	if !sawConversation {
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

// Relocate re-homes a Claude session to newCwd: it writes the .jsonl into
// newCwd's encoded project dir (which is how Claude locates it) AND rewrites the
// cwd field on every line (which is how this tool groups it) so the two agree.
// Copy uses a fresh uuid and keeps the original; move removes it.
func (a ClaudeCodeAgent) Relocate(s Session, newCwd string, asCopy bool) (string, error) {
	src := a.primaryFile(s)
	if src == "" {
		return "", errors.New("session has no file")
	}
	destDir := filepath.Join(claudeProjectsRoot(), claudeEncodeCwd(newCwd))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	id := s.ID
	if asCopy {
		id = uuid.NewString()
	}
	dst := filepath.Join(destDir, id+".jsonl")
	if err := claudeRewriteCwd(src, dst, newCwd); err != nil {
		return "", err
	}
	if !asCopy {
		_ = os.Remove(src) // move: drop the original after writing the new file
	}
	return id, nil
}

// Rename sets the session's title by rewriting its ai-title rows — the same row
// type Claude writes itself, and the one our reader takes the title from.
//
// Verified against real history: Claude re-emits an ai-title row repeatedly (up
// to 700 times in one file) but never with a different value — 0 of 47 files had
// two distinct titles. So "one title value per file" is Claude's own invariant,
// and rewriting every row preserves it rather than leaving a mix. Caveat worth
// knowing: resuming the session may have Claude append its own title again, at
// which point ours is superseded.
func (a ClaudeCodeAgent) Rename(s Session, title string) error {
	path := a.primaryFile(s)
	if path == "" {
		return errors.New("session has no file")
	}
	sessionID := s.ID

	tmp := path + ".asbutler-tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	wrote := false
	lines := 0
	forEachLine(path, func(line string) bool {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			if t, _ := obj["type"].(string); t == "ai-title" {
				obj["aiTitle"] = title
				if b, e := json.Marshal(obj); e == nil {
					line = string(b)
					wrote = true
				}
			}
		}
		fmt.Fprintln(out, line)
		lines++
		return true
	})
	if lines == 0 {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("read no lines from %s", path)
	}
	// A session Claude never titled has no such row; add one so the title sticks.
	if !wrote {
		row, err := json.Marshal(map[string]any{"type": "ai-title", "aiTitle": title, "sessionId": sessionID})
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		fmt.Fprintln(out, string(row))
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename over the original only once the replacement is complete on disk.
	return os.Rename(tmp, path)
}

// claudeRewriteCwd streams src to dst, setting each JSON line's cwd to newCwd.
// Non-JSON lines are copied verbatim. Errors (and reading nothing) return an
// error so a move never deletes the source after a failed write.
func claudeRewriteCwd(src, dst, newCwd string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	lines := 0
	forEachLine(src, func(line string) bool {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			if _, ok := obj["cwd"]; ok {
				obj["cwd"] = newCwd
				if b, e := json.Marshal(obj); e == nil {
					line = string(b)
				}
			}
		}
		fmt.Fprintln(out, line)
		lines++
		return true
	})
	if err := out.Close(); err != nil {
		return err
	}
	if lines == 0 {
		os.Remove(dst)
		return fmt.Errorf("read no lines from %s", src)
	}
	return nil
}

// claudeEncodeCwd maps a working directory to Claude's project dir name: every
// '/' and '.' becomes '-'.
func claudeEncodeCwd(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
}

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
