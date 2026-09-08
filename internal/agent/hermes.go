package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO — keeps cross-compile trivial)
)

// HermesAgent reads Hermes sessions from per-profile SQLite state.db files
// ($HERMES_HOME/state.db and profiles/*/state.db). A session is a row in the
// `sessions` table. DBs are opened read-only (mode=ro, WAL-safe); deletion goes
// through the `hermes` CLI, never raw SQL (which would corrupt the FTS index).
type HermesAgent struct{}

func (HermesAgent) Name() string { return "Hermes" }

// hermesHome resolves HERMES_HOME (env), falling back to ~/.hermes.
func hermesHome() string {
	if h := os.Getenv("HERMES_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hermes")
}

// dbEntry is one state.db and the profile it belongs to.
type dbEntry struct {
	path    string
	profile string // "default" for the root state.db, else the profile dir name
}

// databases enumerates the root state.db plus each profiles/*/state.db. It
// skips state-snapshots/ (pre-update backups, not live session stores).
func hermesDatabases() []dbEntry {
	root := hermesHome()
	if root == "" {
		return nil
	}
	var out []dbEntry
	if fileExists(filepath.Join(root, "state.db")) {
		out = append(out, dbEntry{path: filepath.Join(root, "state.db"), profile: "default"})
	}
	profileDirs, err := os.ReadDir(filepath.Join(root, "profiles"))
	if err != nil {
		return out
	}
	for _, d := range profileDirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, "profiles", d.Name(), "state.db")
		if fileExists(p) {
			out = append(out, dbEntry{path: p, profile: d.Name()})
		}
	}
	return out
}

// Installed reports true only if some state.db actually holds a CLI session.
// A bare state.db can exist with no sessions (e.g. a default ~/.hermes that was
// never used), which shouldn't surface Hermes with an empty listing.
func (HermesAgent) Installed() bool {
	for _, db := range hermesDatabases() {
		if dbHasCLISession(db.path) {
			return true
		}
	}
	return false
}

// dbHasCLISession does a cheap existence probe (LIMIT 1), not a full scan.
func dbHasCLISession(path string) bool {
	conn, err := openRO(path)
	if err != nil {
		return false
	}
	defer conn.Close()
	var one int
	err = conn.QueryRow(`SELECT 1 FROM sessions WHERE source = 'cli' LIMIT 1`).Scan(&one)
	return err == nil
}

func (a HermesAgent) Scan() []Session {
	var out []Session
	for _, db := range hermesDatabases() {
		out = append(out, a.scanDB(db)...)
	}
	return out
}

// openRO opens a state.db read-only, respecting the gateway's WAL writes. Never
// opened for writing — writing would risk corrupting the FTS index.
func openRO(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro")
}

func (a HermesAgent) scanDB(db dbEntry) []Session {
	conn, err := openRO(db.path)
	if err != nil {
		return nil
	}
	defer conn.Close()

	active := activeSessionIDs(conn, db)       // ids the running gateway holds → locked
	bytesByID, turnsByID := messageStats(conn) // per session: content bytes, conversation turns

	// Only interactive CLI sessions — same scope as Kiro/Claude Code. Channel/
	// cron/imported sessions have no meaningful cwd and would flood the listing.
	rows, err := conn.Query(`SELECT id, title, cwd, message_count, started_at, ended_at, archived FROM sessions WHERE source = 'cli'`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var id string
		var title, cwd sql.NullString
		var msgCount sql.NullInt64
		var startedAt, endedAt sql.NullFloat64
		var archived sql.NullInt64
		if rows.Scan(&id, &title, &cwd, &msgCount, &startedAt, &endedAt, &archived) != nil {
			continue
		}

		c := strings.TrimSpace(cwd.String)
		if c == "" {
			c = "(unknown)"
		}
		ts := endedAt.Float64
		if ts == 0 { // ended_at missing/zero → session never formally ended
			ts = startedAt.Float64
		}
		// Not sessions.message_count: that column is the raw row count of the
		// messages table, a third of which is tool traffic on a real store (554 of
		// 1,663 rows; one session reported 43 against 16 actual turns). The other
		// three agents count conversation turns, so counting rows here would make
		// Hermes the only agent whose number answers a different question.
		mc := turnsByID[id]

		out = append(out, Session{
			ID:           id,
			Agent:        a.Name(),
			Cwd:          c,
			Title:        hermesTitle(title.String, id),
			MessageCount: &mc, // known at scan time; Enrich has nothing to add
			// DB-backed: no file size. Use total message-content bytes (near-
			// complete coverage, unlike the sparse token columns); 0 if none.
			FileSize:   bytesByID[id],
			ModifiedAt: unixToTime(ts),
			Locked:     active[id],
			CacheKey:   db.path + "#" + id,
			Extra:      map[string]string{"profile": db.profile},
		})
	}
	return out
}

// messageStats returns, per session id, the total byte length of its message
// content and its conversation-turn count — one grouped pass over the messages
// table, since both were being derived from it anyway.
//
// Turns are user and assistant rows only. `role='tool'` is excluded to match the
// other three agents: tool traffic is protocol, not conversation.
func messageStats(conn *sql.DB) (bytesByID map[string]int64, turnsByID map[string]int) {
	rows, err := conn.Query(`SELECT session_id, sum(length(content)),
		sum(role IN ('user','assistant')) FROM messages GROUP BY session_id`)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	bytesByID, turnsByID = map[string]int64{}, map[string]int{}
	for rows.Next() {
		var id string
		var size, turns sql.NullInt64
		if rows.Scan(&id, &size, &turns) == nil {
			bytesByID[id] = size.Int64
			turnsByID[id] = int(turns.Int64)
		}
	}
	return bytesByID, turnsByID
}

// Enrich is a no-op for Hermes: title and message_count come straight from the
// sessions row at scan time.
func (HermesAgent) Enrich(s Session) Session { return s }

// Delete removes the session via the Hermes CLI, which cascades to messages and
// the FTS shadow tables (a raw SQL DELETE would leave the index corrupt). The
// profile is selected with -p; the root/default profile takes no flag.
func (HermesAgent) Delete(s Session) error {
	args := []string{"sessions", "delete", s.ID, "-y"}
	// Root/default profile takes no -p flag (bare HERMES_HOME = default).
	if p := s.Extra["profile"]; p != "" && p != "default" {
		args = append(args, "-p", p)
	}
	cmd := exec.Command("hermes", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return &deleteError{msg}
	}
	return nil
}

type deleteError struct{ msg string }

func (e *deleteError) Error() string { return "hermes delete failed: " + e.msg }

// Rename sets the session's title through `hermes sessions rename`, not by
// writing the DB. The profile flag is required for the same reason as in Delete:
// without it the CLI looks in the default HERMES_HOME and reports the session as
// not found, silently doing nothing.
//
// Verified on a real store: titles are not held in an FTS shadow table (only
// messages_fts exists), so a title change carries none of the index-corruption
// risk that makes Delete go through the CLI — but the CLI is still the supported
// path, and consistency beats a special case.
func (HermesAgent) Rename(s Session, title string) error {
	args := []string{"sessions", "rename", s.ID, title}
	if p := s.Extra["profile"]; p != "" && p != "default" {
		args = append(args, "-p", p)
	}
	cmd := exec.Command("hermes", args...)
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return &renameError{msg}
	}
	// The CLI exits 0 while reporting "not found", so the output has to be read.
	if strings.Contains(msg, "not found") {
		return &renameError{msg}
	}
	return nil
}

type renameError struct{ msg string }

func (e *renameError) Error() string { return "hermes rename failed: " + e.msg }

// Relocate is unsupported for Hermes: a session's home is its profile/DB, not a
// cwd, and moving rows across profile DBs isn't a meaningful operation here.
func (HermesAgent) Relocate(s Session, newCwd string, asCopy bool) (string, error) {
	return "", ErrRelocateUnsupported
}

// activeSessionIDs returns the set of session ids the profile's running gateway
// currently holds — those are treated as locked. Empty if the gateway isn't
// live. Session ids live inside gateway_routing.entry_json (a JSON blob), not a
// column, and join 1:1 to sessions.id.
func activeSessionIDs(conn *sql.DB, db dbEntry) map[string]bool {
	if !gatewayLive(db) {
		return nil
	}
	rows, err := conn.Query(`SELECT entry_json FROM gateway_routing`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	active := map[string]bool{}
	for rows.Next() {
		var entry string
		if rows.Scan(&entry) != nil {
			continue
		}
		var m struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(entry), &m) == nil && m.SessionID != "" {
			active[m.SessionID] = true
		}
	}
	return active
}

// gatewayLive reports whether the profile's gateway process is running, by
// reading profiles/<name>/gateway.pid and checking the PID is alive.
func gatewayLive(db dbEntry) bool {
	dir := filepath.Dir(db.path)
	data, err := os.ReadFile(filepath.Join(dir, "gateway.pid"))
	if err != nil {
		return false
	}
	var obj struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(data, &obj) != nil || obj.PID == 0 {
		return false
	}
	return pidAlive(obj.PID)
}

// hermesTitle uses the stored title, falling back to a short-id placeholder
// (titles are frequently empty). Hermes titles are clean TEXT — no JSON-soup
// cleanup needed (unlike Kiro).
func hermesTitle(raw, id string) string {
	t := strings.TrimSpace(raw)
	if t != "" {
		return t
	}
	return "(untitled · " + shortTail(id) + ")"
}

func unixToTime(sec float64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), int64((sec-float64(int64(sec)))*1e9))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Transcript reads a Hermes session's rows in order. Reasoning columns are empty
// in practice, so Turn.Thinking is not populated. A role='tool' row is the
// result of the preceding assistant turn's call.
func (a HermesAgent) Transcript(s Session, opts TranscriptOptions) ([]Turn, error) {
	dbPath, _, ok := strings.Cut(s.CacheKey, "#")
	if !ok {
		return nil, errors.New("session has no database reference")
	}
	conn, err := openRO(dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	rows, err := conn.Query(`SELECT role, content, tool_name, tool_calls, timestamp
		FROM messages WHERE session_id = ? ORDER BY id`, s.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var turns []Message
	var userInput []bool
	for rows.Next() {
		var role string
		var content, toolName, toolCalls sql.NullString
		var ts sql.NullFloat64
		if rows.Scan(&role, &content, &toolName, &toolCalls, &ts) != nil {
			continue
		}
		body := content.String

		if role == "tool" {
			// Attach to the assistant turn that called it.
			if len(turns) > 0 && len(turns[len(turns)-1].Tools) > 0 {
				last := &turns[len(turns)-1]
				call := &last.Tools[len(last.Tools)-1]
				call.OutputBytes = len(body)
				if opts.Bodies {
					call.Output = readableBody(body)
				}
				continue
			}
			// Orphaned result: show it rather than dropping it silently.
			turn := Message{Role: "assistant", Tools: []ToolCall{{
				Name: toolName.String, Summary: summarise(body, toolSummaryRunes), OutputBytes: len(body),
			}}}
			if opts.Bodies {
				turn.Tools[0].Output = readableBody(body)
			}
			turns = append(turns, turn)
			userInput = append(userInput, false)
			continue
		}

		turn := Message{Role: role, Text: strings.TrimSpace(body)}
		if ts.Valid && ts.Float64 != 0 {
			at := unixToTime(ts.Float64).Format(time.RFC3339)
			turn.At = &at
		}
		if tc := toolCalls.String; tc != "" && tc != "null" {
			turn.Tools = append(turn.Tools, hermesToolCalls(tc, opts.Bodies)...)
		}
		turns = append(turns, turn)
		userInput = append(userInput, role == "user")
	}

	ex := groupTurns(turns, func(i int) bool { return userInput[i] })
	return windowTurns(ex, opts), nil
}

// hermesToolCalls parses the tool_calls column, which holds the provider's own
// JSON array; an unrecognised shape yields one opaque entry rather than nothing.
func hermesToolCalls(raw string, bodies bool) []ToolCall {
	var calls []struct {
		Function struct {
			Name string `json:"name"`
			Args string `json:"arguments"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(raw), &calls) != nil {
		return []ToolCall{{Name: "tool", Summary: summarise(raw, toolSummaryRunes), ArgsBytes: len(raw)}}
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		name := c.Function.Name
		if name == "" {
			name = c.Name
		}
		args := c.Function.Args
		call := ToolCall{Name: name, Summary: summariseArgs(args, toolSummaryRunes), ArgsBytes: len(args)}
		if bodies {
			call.Args = readableBody(args)
		}
		out = append(out, call)
	}
	return out
}
