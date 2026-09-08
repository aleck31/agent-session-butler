package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newKiroDB writes a Kiro data.sqlite3 with the v1 store's schema. The table is
// named conversations_v2 in Kiro's database while its CLI calls that store v1;
// the confusion is Kiro's and the test keeps its names.
func newKiroDB(t *testing.T, home string, rows map[string]kiroV1Row) {
	t.Helper()
	path := kiroDBPathIn(t, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversations_v2 (key TEXT NOT NULL,
		conversation_id TEXT NOT NULL, value TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		PRIMARY KEY (key, conversation_id))`); err != nil {
		t.Fatal(err)
	}
	for id, r := range rows {
		blob, err := json.Marshal(map[string]any{"conversation_id": id, "history": r.History})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO conversations_v2 VALUES (?,?,?,?,?)`,
			r.Cwd, id, string(blob), r.CreatedMs, r.UpdatedMs); err != nil {
			t.Fatal(err)
		}
	}
}

type kiroV1Row struct {
	Cwd                  string
	CreatedMs, UpdatedMs int64
	History              []any
}

// kiroDBPathIn resolves the database path for a sandboxed HOME.
func kiroDBPathIn(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return kiroDBPath()
}

// v1 prompt/response pairs, the shape Kiro stores.
func v1Prompt(text string) any {
	return map[string]any{
		"user": map[string]any{
			"content":   map[string]any{"Prompt": map[string]any{"prompt": text}},
			"timestamp": "2026-01-18T11:57:00Z",
		},
		"assistant": map[string]any{"Response": map[string]any{"content": "an answer"}},
	}
}

func v1ToolUse(purpose, tool, path string) any {
	return map[string]any{
		"user": map[string]any{"content": map[string]any{}},
		"assistant": map[string]any{"ToolUse": map[string]any{
			"content": purpose,
			"tool_uses": []any{map[string]any{
				"id": "tu1", "name": tool,
				"args": map[string]any{"operations": []any{map[string]any{"path": path}}},
			}},
		}},
	}
}

func v1ToolResult(body string) any {
	return map[string]any{
		"user": map[string]any{"content": map[string]any{
			"ToolUseResults": map[string]any{"tool_use_results": []any{
				map[string]any{"tool_use_id": "tu1", "content": []any{map[string]any{"Text": body}}},
			}},
		}},
		"assistant": map[string]any{},
	}
}

// The same id lives in both stores because opening a v1 session copies it to v2
// and leaves the original. Merging them would hide the stale copy, which is the
// one a user wants to clean up.
func TestKiroScanKeepsBothStoresForOneID(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"shared-id": {Cwd: "/proj", UpdatedMs: 1_600_000_000_000, History: []any{v1Prompt("older copy")}},
		"v1-only":   {Cwd: "/proj", UpdatedMs: 1_500_000_000_000, History: []any{v1Prompt("never opened")}},
	})
	// The v2 side of the shared id.
	writeKiroBundle(t, "shared-id", map[string]any{"cwd": "/proj", "title": "newer copy"},
		map[string]string{".jsonl": kiroEvent("Prompt") + "\n"})

	got := (KiroAgent{}).Scan()
	if len(got) != 3 {
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID+"/"+s.Store)
		}
		t.Fatalf("scan: got %v, want 3 rows (two stores for shared-id plus the v1-only one)", ids)
	}

	stores := map[string][]string{}
	for _, s := range got {
		stores[s.ID] = append(stores[s.ID], s.Store)
	}
	if len(stores["shared-id"]) != 2 {
		t.Errorf("shared id: got stores %v, want one row per store", stores["shared-id"])
	}
	for _, s := range got {
		if s.Store != kiroStoreV1 && s.Store != kiroStoreV2 {
			t.Errorf("%s: store is %q, want v1 or v2", s.ID, s.Store)
		}
		// CacheKey must separate the stores or the enrichment cache collides.
		if s.Store == kiroStoreV1 && !filepath.IsAbs(s.CacheKey[:1]+"") {
			_ = s
		}
	}
	keys := map[string]bool{}
	for _, s := range got {
		if keys[s.CacheKey] {
			t.Errorf("duplicate CacheKey %q — the two stores would share a cache entry", s.CacheKey)
		}
		keys[s.CacheKey] = true
	}
}

// Kiro can be installed with only the old store populated.
func TestKiroInstalledWithOnlyTheV1Store(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "/x", UpdatedMs: 1, History: []any{v1Prompt("q")}},
	})
	if !(KiroAgent{}).Installed() {
		t.Error("Installed() is false with a populated v1 store and no v2 directory")
	}
}

// The count uses the same rule as v2 — prompts plus assistant messages — because
// the same session often exists in both stores and the numbers must be comparable.
func TestKiroV1EnrichCountsLikeV2(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "/x", UpdatedMs: 1, History: []any{
			v1Prompt("first question"),            // 1 user + 1 assistant
			v1ToolUse("reading", "fs_read", "/f"), // 1 assistant
			v1ToolResult("file body"),             // no turn of its own
			v1Prompt("second question"),           // 1 user + 1 assistant
		}},
	})
	s := (KiroAgent{}).Scan()[0]
	if s.MessageCount != nil {
		t.Error("scan must leave the count nil: the blob is hundreds of KB")
	}
	e := (KiroAgent{}).Enrich(s)
	if e.MessageCount == nil || *e.MessageCount != 5 {
		t.Errorf("messageCount: got %v, want 5", e.MessageCount)
	}
	if e.Title != "first question" {
		t.Errorf("title: got %q, want the first prompt", e.Title)
	}
}

func TestKiroV1Transcript(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "/x", UpdatedMs: 1, History: []any{
			v1Prompt("do the thing"),
			v1ToolUse("reading first", "fs_read", "/a/b.md"),
			v1ToolResult("the file body"),
			v1Prompt("and now this"),
		}},
	})
	s := (KiroAgent{}).Scan()[0]

	turns, err := (KiroAgent{}).Transcript(s, TranscriptOptions{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (one per prompt)", len(turns))
	}

	var tool *ToolCall
	for _, m := range turns[0].Messages {
		if len(m.Tools) > 0 {
			tool = &m.Tools[0]
		}
	}
	if tool == nil {
		t.Fatal("the tool use did not reach a message")
	}
	if tool.Name != "fs_read" || tool.Summary != "/a/b.md" {
		t.Errorf("tool: got %+v", tool)
	}
	// The result arrives on a later entry, so pairing has to look backwards.
	if tool.OutputBytes != len("the file body") {
		t.Errorf("outputBytes: got %d, want %d", tool.OutputBytes, len("the file body"))
	}
	// v1 records a timestamp per entry; v2 records none.
	if turns[0].Messages[0].At == nil {
		t.Error("v1 has timestamps and they should be carried through")
	}
}

// v1 stores no title, so renaming would mean rewriting the first user message.
func TestKiroV1RenameIsRefusedWithAReason(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "/x", UpdatedMs: 1, History: []any{v1Prompt("q")}},
	})
	s := (KiroAgent{}).Scan()[0]
	err := (KiroAgent{}).Rename(s, "a new title")
	if err == nil {
		t.Fatal("expected v1 rename to be refused")
	}
	if !errors.Is(err, ErrRenameUnsupported) {
		t.Errorf("got %v, want it to wrap ErrRenameUnsupported", err)
	}
	// The message has to say why, or the refusal looks like a missing feature.
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error %q should explain that the store has no title field", err)
	}
}

func TestKiroV1RelocateIsRefused(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "/x", UpdatedMs: 1, History: []any{v1Prompt("q")}},
	})
	s := (KiroAgent{}).Scan()[0]
	if _, err := (KiroAgent{}).Relocate(s, "/new", false); !errors.Is(err, ErrRelocateUnsupported) {
		t.Errorf("got %v, want ErrRelocateUnsupported", err)
	}
}

func TestKiroV1EmptyCwdBecomesUnknown(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "", UpdatedMs: 1, History: []any{v1Prompt("q")}},
	})
	if got := (KiroAgent{}).Scan()[0].Cwd; got != "(unknown)" {
		t.Errorf("cwd: got %q, want (unknown)", got)
	}
}

func TestKiroV1ModifiedAtFallsBackToCreated(t *testing.T) {
	home := t.TempDir()
	newKiroDB(t, home, map[string]kiroV1Row{
		"a": {Cwd: "/x", CreatedMs: 1_400_000_000_000, History: []any{v1Prompt("q")}},
	})
	if got := (KiroAgent{}).Scan()[0].ModifiedAt.UnixMilli(); got != 1_400_000_000_000 {
		t.Errorf("modifiedAt: got %d, want created_at", got)
	}
}
