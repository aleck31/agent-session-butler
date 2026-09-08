package agent

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Kiro keeps two session stores and the same id can appear in both: opening a v1
// session copies it to v2 under the same id, leaving the original behind. This
// file covers v1, rows in the `conversations_v2` table of Kiro's SQLite database —
// the name is Kiro's, and its CLI calls that store v1. See ADR-0006.
const (
	kiroStoreV1 = "v1"
	kiroStoreV2 = "v2"
)

// kiroDBPath locates Kiro's database. Only macOS is verified; the other paths
// follow each platform's convention and are best-effort.
func kiroDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "kiro-cli", "data.sqlite3")
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "kiro-cli", "data.sqlite3")
		}
		return filepath.Join(home, "AppData", "Local", "kiro-cli", "data.sqlite3")
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "kiro-cli", "data.sqlite3")
		}
		return filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3")
	}
}

// scanV1 reads the v1 store. Opened read-only; deletion goes through
// `kiro-cli chat -d`, never a write here.
func (a KiroAgent) scanV1() []Session {
	dbPath := kiroDBPath()
	if dbPath == "" || !fileExists(dbPath) {
		return nil
	}
	conn, err := openRO(dbPath)
	if err != nil {
		return nil
	}
	defer conn.Close()

	rows, err := conn.Query(`SELECT conversation_id, key, updated_at, created_at,
		length(value) FROM conversations_v2`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var id string
		var cwd sql.NullString
		var updated, created, size sql.NullInt64
		if rows.Scan(&id, &cwd, &updated, &created, &size) != nil {
			continue
		}
		c := strings.TrimSpace(cwd.String)
		if c == "" {
			c = "(unknown)"
		}
		ms := updated.Int64
		if ms == 0 {
			ms = created.Int64
		}
		out = append(out, Session{
			ID:    id,
			Agent: a.Name(),
			Cwd:   c,
			Store: kiroStoreV1,
			// Title and count need the conversation blob, which runs to hundreds of
			// KB, so both wait for Enrich.
			Title:        "(untitled · " + shortTail(id) + ")",
			MessageCount: nil,
			FileSize:     size.Int64,
			ModifiedAt:   msToTime(ms),
			// v1 is not a live store — Kiro copies out of it rather than writing to
			// it — so there is no lock to check.
			Locked:   false,
			CacheKey: dbPath + "#v1#" + id,
			Extra:    map[string]string{"store": kiroStoreV1, "db": dbPath},
		})
	}
	return out
}

// kiroV1Blob fetches one v1 conversation's JSON. Separate from scanning because
// it is the expensive part: hundreds of KB per session.
func kiroV1Blob(s Session) (map[string]json.RawMessage, error) {
	dbPath := s.Extra["db"]
	if dbPath == "" {
		return nil, fmt.Errorf("session has no database reference")
	}
	conn, err := openRO(dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var raw string
	if err := conn.QueryRow(`SELECT value FROM conversations_v2 WHERE conversation_id = ?`,
		s.ID).Scan(&raw); err != nil {
		return nil, err
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return nil, err
	}
	return blob, nil
}

// kiroV1Entry is one history entry: a user side and an assistant side, unlike v2's
// flat event stream.
type kiroV1Entry struct {
	User struct {
		Content   map[string]json.RawMessage `json:"content"`
		Timestamp string                     `json:"timestamp"`
	} `json:"user"`
	Assistant map[string]json.RawMessage `json:"assistant"`
}

func kiroV1History(blob map[string]json.RawMessage) []kiroV1Entry {
	var history []kiroV1Entry
	if raw, ok := blob["history"]; ok {
		_ = json.Unmarshal(raw, &history)
	}
	return history
}

// enrichV1 fills the title and message count. The count uses the same rule as v2 —
// user prompts plus assistant messages — so the two stores' numbers are comparable,
// which matters because the same session often exists in both.
func (a KiroAgent) enrichV1(s Session) Session {
	blob, err := kiroV1Blob(s)
	if err != nil {
		zero := 0
		s.MessageCount = &zero
		return s
	}
	history := kiroV1History(blob)

	count := 0
	title := ""
	for _, e := range history {
		if raw, ok := e.User.Content["Prompt"]; ok {
			count++
			if title == "" {
				var p struct {
					Prompt string `json:"prompt"`
				}
				if json.Unmarshal(raw, &p) == nil {
					title = p.Prompt
				}
			}
		}
		if len(e.Assistant) > 0 {
			count++
		}
	}
	s.MessageCount = &count
	if t := clampTitle(title); t != "" {
		s.Title = t
	}
	return s
}

// transcriptV1 renders a v1 conversation. Its history is already paired, so unlike
// v2 there is no matching of tool calls to results across events.
func (a KiroAgent) transcriptV1(s Session, opts TranscriptOptions) ([]Turn, error) {
	blob, err := kiroV1Blob(s)
	if err != nil {
		return nil, err
	}

	var messages []Message
	var userInput []bool
	for _, e := range kiroV1History(blob) {
		// The user side is either a prompt someone typed or the results of the
		// tools the previous assistant message asked for.
		if raw, ok := e.User.Content["Prompt"]; ok {
			var p struct {
				Prompt string `json:"prompt"`
			}
			_ = json.Unmarshal(raw, &p)
			m := Message{Role: "user", Text: strings.TrimSpace(p.Prompt)}
			if at := kiroV1Time(e.User.Timestamp); at != "" {
				m.At = &at
			}
			messages = append(messages, m)
			userInput = append(userInput, true)
		}

		m := Message{Role: "assistant"}
		if at := kiroV1Time(e.User.Timestamp); at != "" {
			m.At = &at
		}
		for kind, raw := range e.Assistant {
			switch kind {
			case "Response":
				var r struct {
					Content string `json:"content"`
				}
				if json.Unmarshal(raw, &r) == nil {
					m.Text = appendBlock(m.Text, r.Content)
				}
			case "ToolUse":
				var tu struct {
					Content  string `json:"content"`
					ToolUses []struct {
						ID   string          `json:"id"`
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"tool_uses"`
				}
				if json.Unmarshal(raw, &tu) != nil {
					continue
				}
				m.Text = appendBlock(m.Text, tu.Content)
				for _, u := range tu.ToolUses {
					args := string(u.Args)
					call := ToolCall{
						Name: u.Name, Summary: summariseArgs(args, toolSummaryRunes), ArgsBytes: len(args),
					}
					if opts.Bodies {
						call.Args = readableBody(args)
					}
					m.Tools = append(m.Tools, call)
				}
			}
		}
		if m.Text != "" || len(m.Tools) > 0 {
			messages = append(messages, m)
			userInput = append(userInput, false)
		}

		// Tool results arrive on the *next* entry's user side, so attach them to the
		// assistant message just recorded.
		if raw, ok := e.User.Content["ToolUseResults"]; ok {
			kiroV1AttachResults(messages, raw, opts.Bodies)
		}
	}

	turns := groupTurns(messages, func(i int) bool { return userInput[i] })
	return windowTurns(turns, opts), nil
}

// kiroV1AttachResults puts tool output onto the most recent message that is still
// waiting for it.
func kiroV1AttachResults(messages []Message, raw json.RawMessage, bodies bool) {
	var res struct {
		Results []struct {
			ToolUseID string `json:"tool_use_id"`
			Content   []struct {
				Text string `json:"Text"`
			} `json:"content"`
		} `json:"tool_use_results"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return
	}
	for _, r := range res.Results {
		var body strings.Builder
		for _, c := range r.Content {
			body.WriteString(c.Text)
		}
		// Walk back to the last message with an unfilled tool call.
		for i := len(messages) - 1; i >= 0; i-- {
			filled := false
			for j := range messages[i].Tools {
				if messages[i].Tools[j].OutputBytes == 0 {
					messages[i].Tools[j].OutputBytes = body.Len()
					if bodies {
						messages[i].Tools[j].Output = readableBody(body.String())
					}
					filled = true
					break
				}
			}
			if filled || len(messages[i].Tools) > 0 {
				break
			}
		}
	}
}

// kiroV1Time normalises a v1 timestamp to RFC3339, or returns "" when absent.
func kiroV1Time(ts string) string {
	if ts == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return ""
}

// deleteV1 removes a v1 session through Kiro's own CLI, targeting only that store
// so a v2 copy under the same id survives. Writing the database directly is out:
// it is Kiro's, and the same reasoning applies as for Hermes (ADR-0001 D3).
func (KiroAgent) deleteV1(s Session) error {
	cmd := exec.Command("kiro-cli", "chat", "--delete-session", s.ID, "--session-source", kiroStoreV1)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("kiro delete failed: %s", lastLine(msg))
	}
	return nil
}
