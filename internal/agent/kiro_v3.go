package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kiro's v3 engine keeps each session as a directory, sessions/<sha256(cwd)[:16]>/<id>/, with
// session.json (title, workspacePaths) and messages.jsonl. The id is the directory name: cli_<uuid>_<suffix>
// when derived from a v2 session, sess_<uuid> when new. Each hash dir's .index is a cache Kiro rebuilds.
const kiroStoreV3 = "v3"

func kiroSessionsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kiro", "sessions")
}

type kiroV3Meta struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	WorkspacePaths []string `json:"workspacePaths"`
}

func (a KiroAgent) scanV3() []Session {
	root := kiroSessionsRoot()
	hashes, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Session
	for _, h := range hashes {
		if !h.IsDir() || h.Name() == "cli" { // cli/ is the v2 store
			continue
		}
		dirs, err := os.ReadDir(filepath.Join(root, h.Name()))
		if err != nil {
			continue
		}
		for _, d := range dirs {
			if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
				if s, ok := a.parseV3(filepath.Join(root, h.Name(), d.Name())); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func (a KiroAgent) parseV3(dir string) (Session, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return Session{}, false
	}
	var meta kiroV3Meta
	if json.Unmarshal(data, &meta) != nil {
		return Session{}, false
	}
	id := meta.ID
	if id == "" {
		id = filepath.Base(dir)
	}
	cwd := "(unknown)"
	if len(meta.WorkspacePaths) > 0 && meta.WorkspacePaths[0] != "" {
		cwd = meta.WorkspacePaths[0]
	}
	title := meta.Title
	cleaned := cleanTitle(&title, strings.TrimPrefix(strings.TrimPrefix(id, "cli_"), "sess_"))

	var size int64
	var newest time.Time
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		if info, err := e.Info(); err == nil {
			size += info.Size()
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})

	return Session{
		ID:         id,
		Agent:      a.Name(),
		Cwd:        cwd,
		Title:      cleaned,
		FileSize:   size,
		ModifiedAt: newest,
		Locked:     kiroLockLive(filepath.Join(dir, ".lock")),
		Store:      kiroStoreV3,
		CacheKey:   dir,
		FilePaths:  []string{dir},
		Extra:      map[string]string{"dir": dir},
	}, true
}

// enrichV3 counts a reply as v2 does, once per model response: v3 writes a row per text segment and
// per tool call, so a run of assistant and tool_call rows up to the next user or tool_result is one.
func (a KiroAgent) enrichV3(s Session) Session {
	var t tally
	inReply := false
	forEachLineBytes(filepath.Join(s.Extra["dir"], "messages.jsonl"), func(line []byte) bool {
		if !completeLine(line) {
			return true
		}
		j, ok := fieldStart(line, "payload")
		if !ok {
			return true
		}
		switch typ, _ := topLevelString(line[j:], "type"); typ {
		case "user":
			t.user++
			inReply = false
		case "tool_result":
			inReply = false
		case "assistant", "tool_call":
			if !inReply {
				t.agent++
				inReply = true
			}
			if typ == "tool_call" {
				t.tools++
			}
		}
		return true
	})
	return t.apply(s)
}

func (a KiroAgent) transcriptV3(s Session, opts TranscriptOptions) ([]Turn, error) {
	path := filepath.Join(s.Extra["dir"], "messages.jsonl")
	if !fileExists(path) {
		return nil, errors.New("session has no conversation file")
	}
	var messages []Message
	var userAt []bool
	pending := map[string][2]int{} // toolCallId → (message, tool) index; results arrive later

	forEachLine(path, func(line string) bool {
		var row struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type       string          `json:"type"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"toolCallId"`
				ToolName   string          `json:"toolName"`
				Args       json.RawMessage `json:"args"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			return true
		}
		p := row.Payload
		var at *string
		if t, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil {
			v := t.Format(time.RFC3339)
			at = &v
		}
		switch p.Type {
		case "user", "assistant":
			var text string
			_ = json.Unmarshal(p.Content, &text)
			messages = append(messages, Message{Role: p.Type, Text: strings.TrimSpace(text), At: at})
			userAt = append(userAt, p.Type == "user")
		case "tool_call":
			if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
				messages = append(messages, Message{Role: "assistant", At: at})
				userAt = append(userAt, false)
			}
			args := string(p.Args)
			call := ToolCall{Name: p.ToolName, Summary: summariseArgs(args, toolSummaryRunes), ArgsBytes: len(args)}
			if opts.Bodies {
				call.Args = readableBody(args)
			}
			m := &messages[len(messages)-1]
			m.Tools = append(m.Tools, call)
			pending[p.ToolCallID] = [2]int{len(messages) - 1, len(m.Tools) - 1}
		case "tool_result":
			idx, ok := pending[p.ToolCallID]
			if !ok {
				return true
			}
			var body string
			if json.Unmarshal(p.Content, &body) != nil {
				body = string(p.Content)
			}
			call := &messages[idx[0]].Tools[idx[1]]
			call.OutputBytes = len(body)
			if opts.Bodies {
				call.Output = readableBody(body)
			}
		}
		return true
	})
	return windowTurns(groupTurns(messages, func(i int) bool { return userAt[i] }), opts), nil
}

// deleteV3 goes through kiro-cli, then checks: it reports "Deleted" even for an id it never had.
func (KiroAgent) deleteV3(s Session) error {
	out, err := runTool("kiro-cli", "chat", "--delete-session", s.ID, "--session-source", kiroStoreV3)
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("kiro delete failed: %s", lastLine(msg))
	}
	if _, err := os.Stat(s.Extra["dir"]); err == nil {
		return fmt.Errorf("kiro delete failed: kiro-cli reported success but %s is still there", s.Extra["dir"])
	}
	return nil
}

// renameV3 writes session.json, then drops the hash dir's index: Kiro lists titles from that cache
// and does not notice a changed file, but rebuilds the index when it is missing.
func (KiroAgent) renameV3(s Session, title string) error {
	dir := s.Extra["dir"]
	if err := kiroPatchJSON(filepath.Join(dir, "session.json"), map[string]any{"title": title}); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(filepath.Dir(dir), ".index", "index.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("title written, but Kiro's index could not be refreshed: %w", err)
	}
	return nil
}
