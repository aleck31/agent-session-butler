package agent

import (
	"database/sql"
	"encoding/json"
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

	active := activeSessionIDs(conn, db)  // ids the running gateway holds → locked
	bytesByID := contentBytes(conn)       // session_id → total message content bytes

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
		mc := int(msgCount.Int64)

		out = append(out, Session{
			ID:           id,
			Agent:        a.Name(),
			Cwd:          c,
			Title:        hermesTitle(title.String, id),
			MessageCount: &mc, // sessions.message_count is authoritative; no Enrich needed
			// DB-backed: no file size. Use total message-content bytes (near-
			// complete coverage, unlike the sparse token columns); 0 if none.
			FileSize:     bytesByID[id],
			ModifiedAt:   unixToTime(ts),
			Locked:       active[id],
			CacheKey:     db.path + "#" + id,
			Extra:        map[string]string{"profile": db.profile},
		})
	}
	return out
}

// contentBytes returns, per session id, the total byte length of its message
// content — computed in one grouped pass over the messages table.
func contentBytes(conn *sql.DB) map[string]int64 {
	rows, err := conn.Query(`SELECT session_id, sum(length(content)) FROM messages GROUP BY session_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n sql.NullInt64
		if rows.Scan(&id, &n) == nil {
			out[id] = n.Int64
		}
	}
	return out
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
	return "(untitled · " + hermesShortID(id) + ")"
}

// hermesShortID returns a short distinguishing suffix of the id. The id is an
// opaque primary key — three formats coexist (timestamp, cron_-prefixed, UUID)
// and MUST NOT be parsed. Every format's most-distinguishing part is its tail
// (leading segments share a date/prefix), so take the last 8 chars verbatim.
func hermesShortID(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
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
