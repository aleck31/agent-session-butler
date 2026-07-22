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

// HermesAgent: Hermes stores sessions in SQLite, not files. Each profile has
// its own state.db:
//
//	$HERMES_HOME/state.db                    — the root/default profile
//	$HERMES_HOME/profiles/<name>/state.db    — one per named profile
//
// A session is a row in the `sessions` table (id, title, cwd, message_count,
// started_at, ended_at, …). We open every state.db read-only (mode=ro respects
// the gateway's WAL writes) and never touch the DB to delete — that goes through
// `hermes sessions delete`, which also clears the FTS shadow tables.
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

// dbEntry is one state.db and the profile it belongs to (empty profile = root).
type dbEntry struct {
	path    string
	profile string // "" for the root/default profile
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
		out = append(out, dbEntry{path: filepath.Join(root, "state.db"), profile: ""})
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

func (HermesAgent) Installed() bool {
	return len(hermesDatabases()) > 0
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

	active := activeSessionIDs(conn, db) // ids the running gateway holds → locked

	rows, err := conn.Query(`SELECT id, title, cwd, message_count, started_at, ended_at, archived, input_tokens, output_tokens FROM sessions`)
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
		var inTokens, outTokens sql.NullInt64
		if rows.Scan(&id, &title, &cwd, &msgCount, &startedAt, &endedAt, &archived, &inTokens, &outTokens) != nil {
			continue
		}

		c := strings.TrimSpace(cwd.String)
		if c == "" {
			c = "(unknown)"
		}
		// ended_at is the session's last activity; fall back to started_at when
		// it's missing/zero (session never formally ended).
		ts := endedAt.Float64
		if ts == 0 {
			ts = startedAt.Float64
		}
		mc := int(msgCount.Int64)

		out = append(out, Session{
			ID:           id,
			Agent:        a.Name(),
			Cwd:          c,
			Title:        hermesTitle(title.String, id),
			MessageCount: &mc, // sessions.message_count is authoritative; no Enrich needed
			// No real per-session byte size (DB-backed). Approximate content
			// volume from tokens (~4 bytes/token) so the usage bar and Size
			// column carry a meaningful magnitude, not a flat 0.
			FileSize:   (inTokens.Int64 + outTokens.Int64) * 4,
			ModifiedAt: unixToTime(ts),
			Locked:       active[id],
			CacheKey:     db.path + "#" + id,
			Extra:        map[string]string{"profile": db.profile},
		})
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
	if p := s.Extra["profile"]; p != "" {
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

// hermesShortID picks a distinguishing suffix. Native ids are
// <date>_<time>_<hash> and imported ones are UUIDs — the leading segment is a
// shared date/prefix, so use the trailing hash after the last '_' (or the UUID
// head as a fallback) to keep untitled rows apart.
func hermesShortID(id string) string {
	if i := strings.LastIndexByte(id, '_'); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	if len(id) > 8 {
		return id[:8]
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
