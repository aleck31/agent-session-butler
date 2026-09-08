package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexRow is one fixture row of the `threads` index.
type codexRow struct {
	id        string
	cwd       string
	title     string
	name      any // nil → NULL
	source    string
	tsource   string
	createdMs int64
	updatedMs int64
	rollout   string // written verbatim into rollout_path; "" → empty
	body      string // rollout file contents; written only if rollout != ""
}

// codexSandbox points CODEX_HOME at a temp dir and returns it.
func codexSandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	return home
}

// newCodexDB writes a state_<version>.sqlite with the columns the agent reads.
// The real schema has ~40 columns; only the ones we query need to exist.
func newCodexDB(t *testing.T, home string, version int, rows []codexRow) string {
	t.Helper()
	path := filepath.Join(home, fmt.Sprintf("state_%d.sqlite", version))
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, cwd TEXT NOT NULL,
		title TEXT NOT NULL, name TEXT, source TEXT NOT NULL, thread_source TEXT,
		created_at_ms INTEGER, updated_at_ms INTEGER, archived INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		rollout := r.rollout
		if rollout != "" && r.body != "" {
			if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rollout, []byte(r.body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO threads
			(id, rollout_path, cwd, title, name, source, thread_source, created_at_ms, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			r.id, rollout, r.cwd, r.title, r.name, r.source, r.tsource, r.createdMs, r.updatedMs); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// rolloutPath builds a path shaped like Codex's real layout.
func rolloutPath(home, day, id string) string {
	return filepath.Join(home, "sessions", "2026", "08", day, "rollout-2026-08-"+day+"T12-00-00-"+id+".jsonl")
}

func codexMsg(role, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "message", "role": role,
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		},
	})
	return string(b)
}

func codexOther(itemType string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "response_item",
		"payload": map[string]any{"type": itemType},
	})
	return string(b)
}

// Every session is in scope, subagents included: they carry a real cwd, occupy
// real bytes, and Codex has no retention policy that would ever reclaim them.
func TestCodexScanIncludesSubagentSessions(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{
		{id: "cli1", cwd: "/proj", title: "typed in a terminal",
			source: `"cli"`, tsource: "user", updatedMs: 3000},
		{id: "vsc1", cwd: "/proj", title: "started in the desktop app",
			source: `"vscode"`, updatedMs: 2000},
		{id: "grd1", cwd: "/proj", title: "guardian review",
			source: `{"subagent":{"other":"guardian"}}`, tsource: "subagent", updatedMs: 1000},
		{id: "spw1", cwd: "/proj", title: "spawned child",
			source:  `{"subagent":{"thread_spawn":{"parent_thread_id":"cli1","depth":1}}}`,
			tsource: "subagent", updatedMs: 500},
	})

	got := (CodexAgent{}).Scan()
	if len(got) != 4 {
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		t.Fatalf("scan: got %v, want all four session kinds", ids)
	}
	for _, s := range got {
		if s.Agent != "Codex" {
			t.Errorf("%s agent: got %q, want Codex", s.ID, s.Agent)
		}
		if s.Cwd != "/proj" {
			t.Errorf("%s cwd: got %q", s.ID, s.Cwd)
		}
	}
}

// The state DB filename carries a schema version Codex bumps. Scanning must
// follow the newest, or a Codex upgrade silently leaves us reading stale state.
func TestCodexPicksTheHighestVersionedStateDB(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{{id: "old", cwd: "/old", title: "stale", source: `"cli"`, updatedMs: 1}})
	newCodexDB(t, home, 12, []codexRow{{id: "new", cwd: "/new", title: "current", source: `"cli"`, updatedMs: 1}})
	// A double-digit version must beat a single digit — string ordering would not.
	if got := filepath.Base(codexStateDB()); got != "state_12.sqlite" {
		t.Fatalf("codexStateDB = %q, want state_12.sqlite", got)
	}

	sessions := (CodexAgent{}).Scan()
	if len(sessions) != 1 || sessions[0].ID != "new" {
		t.Errorf("scan read the wrong db: %+v", sessions)
	}
}

// Sibling DBs (logs_2, queue_1, memories_1) must not be mistaken for the state DB.
func TestCodexIgnoresSiblingDatabases(t *testing.T) {
	home := codexSandbox(t)
	for _, name := range []string{"logs_2.sqlite", "queue_1.sqlite", "memories_1.sqlite", "goals_1.sqlite", "state.sqlite"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("not a state db"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := codexStateDB(); got != "" {
		t.Errorf("codexStateDB = %q, want empty (no state_<n>.sqlite present)", got)
	}
	newCodexDB(t, home, 5, []codexRow{{id: "a", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1}})
	if got := filepath.Base(codexStateDB()); got != "state_5.sqlite" {
		t.Errorf("codexStateDB = %q, want state_5.sqlite", got)
	}
}

func TestCodexHomeEnvOverridesDefault(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("CODEX_HOME", "")
	if got := codexHome(); got != filepath.Join(home, ".codex") {
		t.Errorf("default: got %q, want ~/.codex", got)
	}
	t.Setenv("CODEX_HOME", "/custom/codex")
	if got := codexHome(); got != "/custom/codex" {
		t.Errorf("env override: got %q", got)
	}
}

// Size comes from the rollout file, since the index has no byte count.
func TestCodexSizeComesFromTheRolloutFile(t *testing.T) {
	home := codexSandbox(t)
	body := codexMsg("user", "hello") + "\n"
	newCodexDB(t, home, 5, []codexRow{
		{id: "has", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1,
			rollout: rolloutPath(home, "26", "has"), body: body},
		{id: "gone", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1,
			rollout: filepath.Join(home, "sessions", "missing.jsonl")},
		{id: "norollout", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1},
	})

	sizes := map[string]int64{}
	for _, s := range (CodexAgent{}).Scan() {
		sizes[s.ID] = s.FileSize
	}
	if sizes["has"] != int64(len(body)) {
		t.Errorf("has: got %d bytes, want %d", sizes["has"], len(body))
	}
	// A row whose file is already gone is still real state that `codex delete`
	// should clean up, so it is listed at 0 bytes rather than dropped.
	if sizes["gone"] != 0 {
		t.Errorf("gone: got %d bytes, want 0", sizes["gone"])
	}
	if len(sizes) != 3 {
		t.Errorf("got %d sessions, want 3 (none dropped for a missing file)", len(sizes))
	}
}

// updated_at_ms is the modification time; created_at_ms is the fallback.
func TestCodexModifiedAtFallsBackToCreatedAt(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{
		{id: "updated", cwd: "/x", title: "t", source: `"cli"`, createdMs: 1000, updatedMs: 5000},
		{id: "neveronce", cwd: "/x", title: "t", source: `"cli"`, createdMs: 2000},
		{id: "neither", cwd: "/x", title: "t", source: `"cli"`},
	})

	times := map[string]int64{}
	zero := map[string]bool{}
	for _, s := range (CodexAgent{}).Scan() {
		times[s.ID] = s.ModifiedAt.UnixMilli()
		zero[s.ID] = s.ModifiedAt.IsZero()
	}
	if times["updated"] != 5000 {
		t.Errorf("updated: got %d, want 5000", times["updated"])
	}
	if times["neveronce"] != 2000 {
		t.Errorf("neveronce: got %d, want created_at_ms 2000", times["neveronce"])
	}
	if !zero["neither"] {
		t.Errorf("neither: want the zero time, got %v", times["neither"])
	}
}

func TestCodexEmptyCwdBecomesUnknown(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{
		{id: "blank", cwd: "   ", title: "t", source: `"cli"`, updatedMs: 1},
		{id: "empty", cwd: "", title: "t", source: `"cli"`, updatedMs: 1},
	})
	for _, s := range (CodexAgent{}).Scan() {
		if s.Cwd != "(unknown)" {
			t.Errorf("%s cwd: got %q, want %q", s.ID, s.Cwd, "(unknown)")
		}
	}
}

// Enrich counts conversation turns only: assistant messages plus user messages
// the human actually typed. Reasoning, tool traffic and `developer` messages are
// protocol, not turns.
func TestCodexEnrichCountsOnlyConversationTurns(t *testing.T) {
	home := codexSandbox(t)
	body := strings.Join([]string{
		codexMsg("developer", "<personality_spec> system injected"),
		codexMsg("user", "<environment_context>\n  <cwd>/x</cwd>\n"),
		codexMsg("user", "a real question"),
		codexMsg("assistant", "an answer"),
		codexOther("reasoning"),
		codexOther("function_call"),
		codexOther("function_call_output"),
		codexOther("custom_tool_call"),
		codexMsg("user", "<turn_aborted>"),
		codexMsg("user", "<subagent_notification>"),
		codexMsg("user", "another real question"),
		codexMsg("assistant", "another answer"),
		`{"type":"event_msg","payload":{"type":"whatever"}}`,
		`{"type":"turn_context","payload":{}}`,
		`not json`,
	}, "\n") + "\n"

	newCodexDB(t, home, 5, []codexRow{
		{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1,
			rollout: rolloutPath(home, "26", "s1"), body: body},
	})

	got := (CodexAgent{}).Scan()
	if got[0].MessageCount != nil {
		t.Error("scan must leave MessageCount nil (enriched lazily)")
	}
	e := (CodexAgent{}).Enrich(got[0])
	// 2 real user + 2 assistant. The 3 injected user wrappers and the developer
	// message do not count.
	if e.MessageCount == nil || *e.MessageCount != 4 {
		t.Errorf("messageCount: got %v, want 4", e.MessageCount)
	}
}

// Injected wrappers are matched by name, not by "starts with any <tag>", so a
// real message beginning with angle brackets still counts as a turn.
func TestCodexEnrichCountsRealMessagesStartingWithAngleBrackets(t *testing.T) {
	home := codexSandbox(t)
	body := strings.Join([]string{
		codexMsg("user", "<div> why does this render wrong?"),
		codexMsg("user", "<environment_context> injected"),
	}, "\n") + "\n"
	newCodexDB(t, home, 5, []codexRow{
		{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1,
			rollout: rolloutPath(home, "26", "s1"), body: body},
	})

	e := (CodexAgent{}).Enrich((CodexAgent{}).Scan()[0])
	if e.MessageCount == nil || *e.MessageCount != 1 {
		t.Errorf("messageCount: got %v, want 1 (the <div> question counts, the wrapper does not)", e.MessageCount)
	}
}

func TestCodexEnrichWithoutARolloutFileIsZero(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1}})
	e := (CodexAgent{}).Enrich((CodexAgent{}).Scan()[0])
	if e.MessageCount == nil || *e.MessageCount != 0 {
		t.Errorf("messageCount: got %v, want 0", e.MessageCount)
	}
}

// `title` holds the raw first prompt (tens of KB in practice), `name` is the
// user-assigned label and is usually NULL.
func TestCodexTitlePrefersNameThenClampsTheRawPrompt(t *testing.T) {
	long := strings.Repeat("测", 200)
	for name, tc := range map[string]struct {
		rowName any
		title   string
		want    string
	}{
		"name wins":        {"my session", "a very long raw prompt", "my session"},
		"blank name":       {"   ", "the raw prompt", "the raw prompt"},
		"null name":        {nil, "the raw prompt", "the raw prompt"},
		"collapses lines":  {nil, "line one\nline two", "line one line two"},
		"nothing at all":   {nil, "", "(untitled · 4af1ae06)"},
		"clamps long text": {nil, long, string([]rune(long)[:80]) + "…"},
	} {
		t.Run(name, func(t *testing.T) {
			home := codexSandbox(t)
			newCodexDB(t, home, 5, []codexRow{
				{id: "0199aaaa-bbbb-4af1ae06", cwd: "/x", title: tc.title, name: tc.rowName,
					source: `"cli"`, updatedMs: 1},
			})
			got := (CodexAgent{}).Scan()[0].Title
			if got != tc.want {
				t.Errorf("title: got %q, want %q", got, tc.want)
			}
		})
	}
}

// Codex has no per-session lock (only a global 0-byte coordination file), so
// nothing is reported as locked; `codex delete` is what refuses a live session.
func TestCodexSessionsAreNeverReportedLocked(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1}})
	if (CodexAgent{}).Scan()[0].Locked {
		t.Error("locked: got true, want false")
	}
}

// The CacheKey must be db-scoped so a schema bump can't collide ids across DBs.
func TestCodexCacheKeyIsDBScoped(t *testing.T) {
	home := codexSandbox(t)
	dbPath := newCodexDB(t, home, 5, []codexRow{{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1}})
	got := (CodexAgent{}).Scan()[0].CacheKey
	if got != dbPath+"#s1" {
		t.Errorf("cacheKey: got %q, want %q", got, dbPath+"#s1")
	}
}

// ADR-0003: cwd lives in the read-only index as well as the rollout file, with
// no CLI to change it — rewriting one side would leave the two disagreeing.
func TestCodexRelocateIsUnsupported(t *testing.T) {
	for _, asCopy := range []bool{false, true} {
		if _, err := (CodexAgent{}).Relocate(Session{ID: "s1"}, "/new", asCopy); !errors.Is(err, ErrRelocateUnsupported) {
			t.Errorf("asCopy=%v: got %v, want ErrRelocateUnsupported", asCopy, err)
		}
	}
}

func TestCodexInstalledRequiresAStateDBWithThreads(t *testing.T) {
	t.Run("no codex home", func(t *testing.T) {
		codexSandbox(t)
		if (CodexAgent{}).Installed() {
			t.Error("Installed() is true with an empty CODEX_HOME")
		}
	})
	t.Run("state db with no threads", func(t *testing.T) {
		home := codexSandbox(t)
		newCodexDB(t, home, 5, nil)
		if (CodexAgent{}).Installed() {
			t.Error("Installed() is true with an empty threads table")
		}
	})
	t.Run("state db with a thread", func(t *testing.T) {
		home := codexSandbox(t)
		newCodexDB(t, home, 5, []codexRow{{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1}})
		if !(CodexAgent{}).Installed() {
			t.Error("Installed() is false though a thread exists")
		}
	})
}

func TestCodexScanToleratesAMissingDB(t *testing.T) {
	codexSandbox(t)
	if got := (CodexAgent{}).Scan(); len(got) != 0 {
		t.Errorf("got %d sessions, want 0", len(got))
	}
}

// Scanning must not write to the state DB: Codex's own CLI is the only thing
// allowed to mutate it, and a stray write could corrupt state it owns.
func TestCodexScanWorksAgainstAReadOnlyFile(t *testing.T) {
	home := codexSandbox(t)
	dbPath := newCodexDB(t, home, 5, []codexRow{
		{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1},
	})
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })

	if got := (CodexAgent{}).Scan(); len(got) != 1 {
		t.Fatalf("scan against a read-only db: got %d sessions, want 1", len(got))
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state_5.sqlite" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("scan left files behind: %v", names)
	}
}

func TestClampTitle(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"short":            {"a title", "a title"},
		"trims":            {"  padded  ", "padded"},
		"collapses \\n":    {"one\ntwo", "one two"},
		"collapses \\r\\n": {"one\r\ntwo\rthree", "one two three"},
		"empty":            {"", ""},
	} {
		if got := clampTitle(tc.in); got != tc.want {
			t.Errorf("%s: clampTitle(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("x", 200)
	if got := clampTitle(long); len([]rune(got)) != 81 || !strings.HasSuffix(got, "…") {
		t.Errorf("clampTitle(long) = %d runes, want 81 ending in an ellipsis", len([]rune(got)))
	}
}

// The version reported to Codex must come from the build, not a literal that has
// to be remembered at release time.
func TestVersionIsNotHardcoded(t *testing.T) {
	if Version == "" {
		t.Error("agent.Version is empty; main should set it")
	}
	data, err := os.ReadFile("codex.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"version": "0.`) {
		t.Error("codex.go hardcodes a version string; use agent.Version")
	}
}
