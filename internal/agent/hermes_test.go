package agent

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// hermesRow is one fixture session row.
type hermesRow struct {
	id        string
	source    string
	title     string
	cwd       string
	msgCount  int
	startedAt float64
	endedAt   float64
	contents  []string // message bodies; their byte length becomes the session size
}

// newHermesDB creates a state.db at path with Hermes' relevant schema and rows.
// Fixtures are written directly; only the production read path is read-only
// (ADR-0001 D3).
func newHermesDB(t *testing.T, path string, rows []hermesRow, routing []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, title TEXT, cwd TEXT,
			message_count INTEGER, started_at REAL, ended_at REAL, archived INTEGER)`,
		`CREATE TABLE messages (session_id TEXT, content TEXT)`,
		`CREATE TABLE gateway_routing (entry_json TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO sessions (id, source, title, cwd, message_count, started_at, ended_at, archived)
			 VALUES (?,?,?,?,?,?,?,0)`,
			r.id, r.source, r.title, r.cwd, r.msgCount, r.startedAt, r.endedAt); err != nil {
			t.Fatal(err)
		}
		for _, c := range r.contents {
			if _, err := db.Exec(`INSERT INTO messages (session_id, content) VALUES (?,?)`, r.id, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, e := range routing {
		if _, err := db.Exec(`INSERT INTO gateway_routing (entry_json) VALUES (?)`, e); err != nil {
			t.Fatal(err)
		}
	}
}

// hermesSandbox points HERMES_HOME at a fresh temp dir.
func hermesSandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	return home
}

// writeGatewayPID drops a gateway.pid next to a profile's state.db.
func writeGatewayPID(t *testing.T, dbPath string, pid int) {
	t.Helper()
	p := filepath.Join(filepath.Dir(dbPath), "gateway.pid")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(`{"pid":%d}`, pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ADR-0001 D4: only source='cli' is in scope. Channel, cron and imported
// sessions belong to a gateway-centric world and would flood the listing.
func TestHermesScanOnlyCLISessions(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"), []hermesRow{
		{id: "a", source: "cli", title: "coding", cwd: "/home/u/proj", startedAt: 1000},
		{id: "b", source: "cron", title: "nightly", startedAt: 1000},
		{id: "c", source: "discord", title: "chat", startedAt: 1000},
		{id: "d", source: "openclaw-import", title: "old", startedAt: 1000},
	}, nil)

	got := (HermesAgent{}).Scan()
	if len(got) != 1 || got[0].ID != "a" {
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		t.Fatalf("scan: got %v, want only the cli session", ids)
	}
	if got[0].Agent != "Hermes" {
		t.Errorf("agent: got %q", got[0].Agent)
	}
	if got[0].Cwd != "/home/u/proj" {
		t.Errorf("cwd: got %q", got[0].Cwd)
	}
}

// ADR-0001 D5: the root state.db is the "default" profile; each profiles/<name>
// is its own. state-snapshots/ is a pre-update backup, not a live store.
func TestHermesScanEnumeratesProfilesAndLabelsDefault(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"),
		[]hermesRow{{id: "root", source: "cli", cwd: "/shared", startedAt: 1}}, nil)
	newHermesDB(t, filepath.Join(home, "profiles", "x-tec", "state.db"),
		[]hermesRow{{id: "tec", source: "cli", cwd: "/shared", startedAt: 2}}, nil)
	newHermesDB(t, filepath.Join(home, "profiles", "x-main", "state.db"),
		[]hermesRow{{id: "main", source: "cli", cwd: "/shared", startedAt: 3}}, nil)
	// A snapshot backup must be ignored.
	newHermesDB(t, filepath.Join(home, "state-snapshots", "state.db"),
		[]hermesRow{{id: "snapshot", source: "cli", cwd: "/shared", startedAt: 4}}, nil)

	got := (HermesAgent{}).Scan()
	profiles := map[string]string{}
	for _, s := range got {
		profiles[s.ID] = s.Extra["profile"]
	}
	if len(got) != 3 {
		t.Fatalf("scan: got %d sessions (%v), want 3 — snapshots must be skipped", len(got), profiles)
	}
	if profiles["root"] != "default" {
		t.Errorf("root db profile: got %q, want %q", profiles["root"], "default")
	}
	if profiles["tec"] != "x-tec" || profiles["main"] != "x-main" {
		t.Errorf("profile labels: got %v", profiles)
	}
	// CacheKey must be db-scoped, or two profiles' same-id sessions would collide.
	keys := map[string]bool{}
	for _, s := range got {
		if keys[s.CacheKey] {
			t.Errorf("duplicate CacheKey %q", s.CacheKey)
		}
		keys[s.CacheKey] = true
	}
}

// ADR-0001 D6: a DB row has no file size, so size is the total byte length of
// its message content. Genuinely empty means 0 — honest, not hidden.
func TestHermesSizeIsMessageContentBytes(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"), []hermesRow{
		{id: "sized", source: "cli", cwd: "/x", startedAt: 1, contents: []string{"hello", "worldly"}},
		{id: "empty", source: "cli", cwd: "/x", startedAt: 1},
	}, nil)

	sizes := map[string]int64{}
	for _, s := range (HermesAgent{}).Scan() {
		sizes[s.ID] = s.FileSize
	}
	if sizes["sized"] != 12 { // len("hello") + len("worldly")
		t.Errorf("sized: got %d bytes, want 12", sizes["sized"])
	}
	if sizes["empty"] != 0 {
		t.Errorf("empty: got %d bytes, want 0", sizes["empty"])
	}
}

// ended_at is the modification time; a session that never formally ended falls
// back to started_at, and one with neither gets the zero time.
func TestHermesModifiedAtFallsBackToStartedAt(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"), []hermesRow{
		{id: "ended", source: "cli", cwd: "/x", startedAt: 1000, endedAt: 2000},
		{id: "running", source: "cli", cwd: "/x", startedAt: 1500},
		{id: "neither", source: "cli", cwd: "/x"},
	}, nil)

	times := map[string]time.Time{}
	for _, s := range (HermesAgent{}).Scan() {
		times[s.ID] = s.ModifiedAt
	}
	if got := times["ended"].Unix(); got != 2000 {
		t.Errorf("ended: got %d, want ended_at 2000", got)
	}
	if got := times["running"].Unix(); got != 1500 {
		t.Errorf("running: got %d, want started_at 1500", got)
	}
	if !times["neither"].IsZero() {
		t.Errorf("neither: got %v, want the zero time", times["neither"])
	}
}

func TestHermesEmptyCwdBecomesUnknown(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"), []hermesRow{
		{id: "nocwd", source: "cli", startedAt: 1},
		{id: "blankcwd", source: "cli", cwd: "   ", startedAt: 1},
	}, nil)

	for _, s := range (HermesAgent{}).Scan() {
		if s.Cwd != "(unknown)" {
			t.Errorf("%s cwd: got %q, want %q", s.ID, s.Cwd, "(unknown)")
		}
	}
}

// A Hermes session is locked only when its profile's gateway is running AND the
// gateway currently holds that session.
func TestHermesLockNeedsLiveGatewayAndRoutingEntry(t *testing.T) {
	held := `{"session_id":"held"}`

	for name, tc := range map[string]struct {
		pid        int
		writePID   bool
		routing    []string
		wantLocked map[string]bool
	}{
		"live gateway holding it": {
			pid: os.Getpid(), writePID: true, routing: []string{held},
			wantLocked: map[string]bool{"held": true, "idle": false},
		},
		"live gateway, not holding it": {
			pid: os.Getpid(), writePID: true, routing: nil,
			wantLocked: map[string]bool{"held": false, "idle": false},
		},
		"dead gateway with a stale routing entry": {
			pid: deadPID, writePID: true, routing: []string{held},
			wantLocked: map[string]bool{"held": false, "idle": false},
		},
		"no gateway.pid at all": {
			writePID: false, routing: []string{held},
			wantLocked: map[string]bool{"held": false, "idle": false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := hermesSandbox(t)
			dbPath := filepath.Join(home, "state.db")
			newHermesDB(t, dbPath, []hermesRow{
				{id: "held", source: "cli", cwd: "/x", startedAt: 1},
				{id: "idle", source: "cli", cwd: "/x", startedAt: 1},
			}, tc.routing)
			if tc.writePID {
				writeGatewayPID(t, dbPath, tc.pid)
			}

			for _, s := range (HermesAgent{}).Scan() {
				if want := tc.wantLocked[s.ID]; s.Locked != want {
					t.Errorf("%s locked: got %v, want %v", s.ID, s.Locked, want)
				}
			}
		})
	}
}

// Installed() requires an actual CLI session: a bare state.db that was never
// used interactively shouldn't surface Hermes with an empty listing.
func TestHermesInstalledRequiresACLISession(t *testing.T) {
	t.Run("no state.db", func(t *testing.T) {
		hermesSandbox(t)
		if (HermesAgent{}).Installed() {
			t.Error("Installed() is true with no state.db")
		}
	})
	t.Run("state.db with no cli sessions", func(t *testing.T) {
		home := hermesSandbox(t)
		newHermesDB(t, filepath.Join(home, "state.db"),
			[]hermesRow{{id: "cronjob", source: "cron", startedAt: 1}}, nil)
		if (HermesAgent{}).Installed() {
			t.Error("Installed() is true with only non-cli sessions")
		}
	})
	t.Run("cli session in a profile only", func(t *testing.T) {
		home := hermesSandbox(t)
		newHermesDB(t, filepath.Join(home, "profiles", "p", "state.db"),
			[]hermesRow{{id: "a", source: "cli", cwd: "/x", startedAt: 1}}, nil)
		if !(HermesAgent{}).Installed() {
			t.Error("Installed() is false though a profile has a cli session")
		}
	})
}

// HERMES_HOME wins over ~/.hermes.
func TestHermesHomeEnvOverridesDefault(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("HERMES_HOME", "")
	if got := hermesHome(); got != filepath.Join(home, ".hermes") {
		t.Errorf("default: got %q, want ~/.hermes", got)
	}
	t.Setenv("HERMES_HOME", "/custom/hermes")
	if got := hermesHome(); got != "/custom/hermes" {
		t.Errorf("env override: got %q", got)
	}
}

// message_count comes straight off the sessions row, so Enrich has nothing to do.
func TestHermesEnrichIsANoOpAndCountIsSetAtScan(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"),
		[]hermesRow{{id: "a", source: "cli", cwd: "/x", msgCount: 7, startedAt: 1}}, nil)

	s := (HermesAgent{}).Scan()[0]
	if s.MessageCount == nil || *s.MessageCount != 7 {
		t.Fatalf("messageCount at scan: got %v, want 7", s.MessageCount)
	}
	if got := (HermesAgent{}).Enrich(s); got.MessageCount == nil || *got.MessageCount != 7 || got.Title != s.Title {
		t.Errorf("Enrich changed the session: %+v vs %+v", got, s)
	}
}

// ADR-0003 D4: a Hermes session's home is its profile, not a cwd.
func TestHermesRelocateIsUnsupported(t *testing.T) {
	for _, asCopy := range []bool{false, true} {
		_, err := (HermesAgent{}).Relocate(Session{ID: "a"}, "/new", asCopy)
		if !errors.Is(err, ErrRelocateUnsupported) {
			t.Errorf("asCopy=%v: got %v, want ErrRelocateUnsupported", asCopy, err)
		}
	}
}

// ADR-0001 D6: three id formats coexist and the id must never be parsed — the
// short form is just the last 8 characters, verbatim.
func TestShortTailTakesTheTailVerbatim(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"20260722T222900-abc123ef", "abc123ef"},
		{"cron_nightly_report_9f2b1c4d", "9f2b1c4d"},
		{"550e8400-e29b-41d4-a716-446655440000", "55440000"},
		{"short", "short"},
		{"exactly8", "exactly8"},
	} {
		if got := shortTail(tc.in); got != tc.want {
			t.Errorf("shortTail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Hermes titles are clean TEXT — used as-is, with a short-id placeholder when
// empty (titles are frequently blank).
func TestHermesTitle(t *testing.T) {
	if got := hermesTitle("  A real title  ", "id"); got != "A real title" {
		t.Errorf("got %q", got)
	}
	if got := hermesTitle("   ", "abcdef1234"); got != "(untitled · cdef1234)" {
		t.Errorf("got %q", got)
	}
}

// A missing or unreadable DB yields nothing rather than panicking.
func TestHermesScanToleratesAMissingDB(t *testing.T) {
	hermesSandbox(t)
	if got := (HermesAgent{}).Scan(); len(got) != 0 {
		t.Errorf("got %d sessions, want 0", len(got))
	}
}

// Scanning must not write to the DB — a raw write would risk corrupting the FTS
// shadow tables (ADR-0001 D3). Assert by making the file read-only on disk.
func TestHermesScanWorksAgainstAReadOnlyFile(t *testing.T) {
	home := hermesSandbox(t)
	dbPath := filepath.Join(home, "state.db")
	newHermesDB(t, dbPath, []hermesRow{
		{id: "a", source: "cli", cwd: "/x", startedAt: 1, contents: []string{"hi"}},
	}, nil)
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })

	got := (HermesAgent{}).Scan()
	if len(got) != 1 {
		t.Fatalf("scan against a read-only db: got %d sessions, want 1", len(got))
	}
	// No journal/WAL sidecar should have been created either.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "state.db" {
		t.Errorf("scan left files behind: %v", names)
	}
}
