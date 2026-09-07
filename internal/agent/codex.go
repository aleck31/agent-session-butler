package agent

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (see hermes.go)
)

// CodexAgent reads Codex sessions. Codex is a hybrid backend: conversation
// content lives in per-session rollout files
// ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl) while an index of
// every session lives in the `threads` table of $CODEX_HOME/state_<n>.sqlite.
// The index is authoritative — Codex is migrating off the files (see its
// `migrate-rollouts` command), so we scan the DB and only read a rollout when
// asked to count messages.
//
// The DB is opened read-only and deletion shells out to `codex delete`: removing
// a rollout file directly would leave a dangling `threads` row, which Codex's own
// session picker would then list as a session whose content is gone.
type CodexAgent struct{}

func (CodexAgent) Name() string { return "Codex" }

// codexHome resolves CODEX_HOME (env), falling back to ~/.codex.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// codexStateVersion matches the schema-version suffix on the state DB.
var codexStateVersion = regexp.MustCompile(`^state_(\d+)\.sqlite$`)

// codexStateDB returns the highest-versioned state_<n>.sqlite. The suffix is a
// schema version that Codex bumps (state_5 today, alongside logs_2/queue_1), so
// globbing and taking the newest keeps us from going blind on the next bump.
func codexStateDB() string {
	root := codexHome()
	if root == "" {
		return ""
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	best, bestVer := "", -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := codexStateVersion.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, err := strconv.Atoi(m[1]); err == nil && v > bestVer {
			best, bestVer = filepath.Join(root, e.Name()), v
		}
	}
	return best
}

func (CodexAgent) Installed() bool {
	db := codexStateDB()
	if db == "" {
		return false
	}
	conn, err := openRO(db)
	if err != nil {
		return false
	}
	defer conn.Close()
	var one int
	// Cheap existence probe, not a full scan.
	return conn.QueryRow(`SELECT 1 FROM threads LIMIT 1`).Scan(&one) == nil
}

func (a CodexAgent) Scan() []Session {
	dbPath := codexStateDB()
	if dbPath == "" {
		return nil
	}
	conn, err := openRO(dbPath)
	if err != nil {
		return nil
	}
	defer conn.Close()

	// Every session is in scope, including the subagent threads Codex spawns for
	// itself (guardian reviews, spawned children). They carry a real cwd, occupy
	// real bytes, and nothing ever reclaims them — Codex has no retention policy,
	// so hiding them would hide the very garbage this tool exists to surface.
	rows, err := conn.Query(`SELECT id, rollout_path, cwd, title, name,
		created_at_ms, updated_at_ms FROM threads`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var id string
		var rollout, cwd, title, name sql.NullString
		var createdMs, updatedMs sql.NullInt64
		if rows.Scan(&id, &rollout, &cwd, &title, &name, &createdMs, &updatedMs) != nil {
			continue
		}

		c := strings.TrimSpace(cwd.String)
		if c == "" {
			c = "(unknown)"
		}
		ms := updatedMs.Int64
		if ms == 0 {
			ms = createdMs.Int64
		}

		// The rollout file is the only thing with a byte size; a row whose file is
		// already gone reports 0 rather than being dropped, since the row itself is
		// still real state that `codex delete` should clean up.
		var size int64
		var paths []string
		if p := rollout.String; p != "" {
			paths = []string{p}
			if info, err := os.Stat(p); err == nil {
				size = info.Size()
			}
		}

		out = append(out, Session{
			ID:    id,
			Agent: a.Name(),
			Cwd:   c,
			Title: codexTitle(name.String, title.String, id),
			// Counting conversation turns means streaming the rollout file, so it
			// stays lazy; everything else came free from the index.
			MessageCount: nil,
			FileSize:     size,
			ModifiedAt:   msToTime(ms),
			// Codex has no per-session lock: only a global 0-byte
			// thread-writer-locks/.coordination.lock with no PID in it. Deletion goes
			// through `codex delete`, which is the component that knows whether a
			// session is live, so the refusal belongs there rather than guessed here.
			Locked:    false,
			CacheKey:  dbPath + "#" + id,
			FilePaths: paths,
		})
	}
	return out
}

// codexInjectedUserTags are wrapper tags Codex injects as `user` messages —
// environment preambles and control notices, not turns the human typed. Matched
// by name rather than "any leading <tag>", so a real message that happens to
// start with angle brackets still counts.
var codexInjectedUserTags = []string{
	"<environment_context>",
	"<turn_aborted>",
	"<subagent_notification>",
}

// Enrich counts conversation turns by streaming the rollout file: `response_item`
// rows holding a user or assistant message. Reasoning, function calls, tool
// output and `developer` messages are excluded — they aren't turns.
func (a CodexAgent) Enrich(s Session) Session {
	count := 0
	if len(s.FilePaths) > 0 {
		forEachLine(s.FilePaths[0], func(line string) bool {
			var row struct {
				Type    string `json:"type"`
				Payload struct {
					Type    string `json:"type"`
					Role    string `json:"role"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(line), &row) != nil {
				return true
			}
			if row.Type != "response_item" || row.Payload.Type != "message" {
				return true
			}
			switch row.Payload.Role {
			case "assistant":
				count++
			case "user":
				if !codexIsInjected(row.Payload.Content) {
					count++
				}
			}
			return true
		})
	}
	s.MessageCount = &count
	return s
}

// codexIsInjected reports whether a user message is one of Codex's injected
// wrappers rather than something the human typed.
func codexIsInjected(content []struct {
	Text string `json:"text"`
}) bool {
	if len(content) == 0 {
		return false
	}
	t := strings.TrimSpace(content[0].Text)
	for _, tag := range codexInjectedUserTags {
		if strings.HasPrefix(t, tag) {
			return true
		}
	}
	return false
}

// Delete removes the session via `codex delete`, which drops both the rollout
// file and its `threads` row. Removing the file ourselves would leave the index
// pointing at nothing.
func (CodexAgent) Delete(s Session) error {
	cmd := exec.Command("codex", "delete", s.ID)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return &codexDeleteError{msg}
	}
	return nil
}

type codexDeleteError struct{ msg string }

func (e *codexDeleteError) Error() string { return "codex delete failed: " + e.msg }

// Rename sets the session's title through the Codex app-server's JSON-RPC
// `thread/name/set`, never by writing the index. Going through Codex matters for
// more than the read-only rule: the call also updates
// $CODEX_HOME/session_index.jsonl, which a direct DB write would leave stale.
//
// Two things about this API are not what its name suggests, both verified: the
// parameter is `name` but the column it lands in is `title`, not the `threads.name`
// column; and the transport is newline-delimited JSON-RPC, not LSP-style
// Content-Length framing (which the server rejects outright).
func (CodexAgent) Rename(s Session, title string) error {
	return codexSetThreadName(s.ID, title)
}

// codexSetThreadName drives one short-lived `codex app-server` session: handshake,
// set the name, then close stdin so the server flushes and exits on EOF.
func codexSetThreadName(threadID, title string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex app-server: %w", err)
	}
	// Always reap the process, including on an early return.
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()

	send := func(id int, method string, params any) error {
		b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdin, "%s\n", b)
		return err
	}
	if err := send(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "asbutler", "version": "0.7.3"},
	}); err != nil {
		return err
	}
	if err := send(2, "thread/name/set", map[string]any{
		"threadId": threadID, "name": title,
	}); err != nil {
		return err
	}

	// Read until the rename's response arrives; the stream also carries unrelated
	// notifications, which are ignored.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var msg struct {
			ID    int             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID != 2 {
			continue
		}
		if len(msg.Error) > 0 {
			return fmt.Errorf("codex rename failed: %s", msg.Error)
		}
		// Close stdin here rather than in the defer alone: the server persists on
		// EOF, so returning before it sees one can lose the write.
		stdin.Close()
		_ = cmd.Wait()
		return nil
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("codex app-server gave no response: %s", lastLine(msg))
	}
	return errors.New("codex app-server closed without answering the rename")
}

// lastLine keeps an error message to the final, usually most specific, line.
func lastLine(s string) string {
	if i := strings.LastIndexByte(strings.TrimRight(s, "\n"), '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// Relocate is unsupported: cwd is a NOT NULL column in the read-only index as
// well as a field in the rollout file, with no CLI to change it. Rewriting only
// the file would leave the two disagreeing — the failure ADR-0003 D3 exists to
// prevent, except here we cannot write the other side.
func (CodexAgent) Relocate(s Session, newCwd string, asCopy bool) (string, error) {
	return "", ErrRelocateUnsupported
}

// codexTitle picks the best available label. `name` is the user-assigned session
// name and is usually NULL; `title` holds the raw first prompt, which runs to
// tens of KB, so it gets collapsed and clamped like Kiro's.
func codexTitle(name, title, id string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if t := strings.TrimSpace(title); t != "" {
		return clampTitle(t)
	}
	return "(untitled · " + shortTail(id) + ")"
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
