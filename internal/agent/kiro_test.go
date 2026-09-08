package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadPID is above every platform's pid_max, so it never names a live process.
const deadPID = 2147483000

// writeKiroBundle lays down a session bundle under ~/.kiro/sessions/cli/:
// the .json metadata plus whatever extra suffixes are named (".jsonl", ".lock").
func writeKiroBundle(t *testing.T, sid string, meta map[string]any, extras map[string]string) {
	t.Helper()
	dir := kiroSessionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		b, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sid+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for suffix, body := range extras {
		if err := os.WriteFile(filepath.Join(dir, sid+suffix), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func kiroEvent(kind string) string {
	b, _ := json.Marshal(map[string]any{"kind": kind})
	return string(b)
}

// A bundle's files are grouped by file-name stem; size is the whole bundle and
// cwd comes from the .json (the sole cwd↔session mapping).
func TestKiroScanGroupsBundleAndReadsCwdFromJSON(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1",
		map[string]any{"cwd": "/home/u/proj", "title": "My session"},
		map[string]string{".jsonl": kiroEvent("Prompt") + "\n", ".history": "ls\n"})

	got := KiroAgent{}.Scan()
	if len(got) != 1 {
		t.Fatalf("scan: got %d sessions, want 1", len(got))
	}
	s := got[0]
	if s.ID != "sid1" {
		t.Errorf("id: got %q, want %q", s.ID, "sid1")
	}
	if s.Cwd != "/home/u/proj" {
		t.Errorf("cwd: got %q, want %q", s.Cwd, "/home/u/proj")
	}
	if s.Title != "My session" {
		t.Errorf("title: got %q, want %q", s.Title, "My session")
	}
	if len(s.FilePaths) != 3 {
		t.Errorf("filePaths: got %d, want 3 (.json/.jsonl/.history)", len(s.FilePaths))
	}
	// Size is the sum of the bundle, not just the .json.
	var want int64
	for _, p := range s.FilePaths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want += info.Size()
	}
	if s.FileSize != want {
		t.Errorf("fileSize: got %d, want %d (whole bundle)", s.FileSize, want)
	}
	if s.MessageCount != nil {
		t.Error("scan must leave MessageCount nil (enriched lazily)")
	}
}

// Without a .json there is no cwd mapping, so the files aren't a real session.
func TestKiroScanSkipsBundleWithoutMetadata(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "orphaned", nil, map[string]string{".jsonl": kiroEvent("Prompt") + "\n"})
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/x"}, nil)

	got := KiroAgent{}.Scan()
	if len(got) != 1 || got[0].ID != "sid1" {
		t.Fatalf("scan: got %+v, want only sid1", got)
	}
}

func TestKiroScanSkipsUnparsableMetadata(t *testing.T) {
	sandboxHome(t)
	dir := kiroSessionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (KiroAgent{}).Scan(); len(got) != 0 {
		t.Errorf("scan: got %+v, want nothing", got)
	}
}

func TestKiroMissingOrEmptyCwdBecomesUnknown(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "nocwd", map[string]any{"title": "t"}, nil)
	writeKiroBundle(t, "blankcwd", map[string]any{"cwd": "", "title": "t"}, nil)

	for _, s := range (KiroAgent{}).Scan() {
		if s.Cwd != "(unknown)" {
			t.Errorf("%s cwd: got %q, want %q", s.ID, s.Cwd, "(unknown)")
		}
	}
}

// Enrich tallies Prompt and AssistantMessage events only — tool results and
// compaction events aren't conversation messages.
func TestKiroEnrichCountsOnlyPromptAndAssistantMessage(t *testing.T) {
	sandboxHome(t)
	events := strings.Join([]string{
		kiroEvent("Prompt"),
		kiroEvent("ToolUse"),
		kiroEvent("AssistantMessage"),
		kiroEvent("ToolUseResults"),
		kiroEvent("CompactionSummary"),
		kiroEvent("Prompt"),
		"not json",
	}, "\n") + "\n"
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/x"}, map[string]string{".jsonl": events})

	e := KiroAgent{}.Enrich(KiroAgent{}.Scan()[0])
	if e.MessageCount == nil || *e.MessageCount != 3 {
		t.Errorf("messageCount: got %v, want 3 (2 Prompt + 1 AssistantMessage)", e.MessageCount)
	}
}

// A bundle with no .jsonl enriches to 0, not to an error or a nil count.
func TestKiroEnrichWithoutJsonlIsZero(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/x"}, nil)
	e := KiroAgent{}.Enrich(KiroAgent{}.Scan()[0])
	if e.MessageCount == nil || *e.MessageCount != 0 {
		t.Errorf("messageCount: got %v, want 0", e.MessageCount)
	}
}

// A lock file only means "locked" if its PID is a live process — a crashed agent
// leaves a stale lock that must not block cleanup.
func TestKiroLockRequiresALivePID(t *testing.T) {
	for name, tc := range map[string]struct {
		lock string
		want bool
	}{
		"live pid":     {fmt.Sprintf(`{"pid":%d}`, os.Getpid()), true},
		"stale pid":    {fmt.Sprintf(`{"pid":%d}`, deadPID), false},
		"pid zero":     {`{"pid":0}`, false},
		"negative pid": {`{"pid":-1}`, false},
		"no pid field": {`{"host":"x"}`, false},
		"not json":     {`garbage`, false},
	} {
		t.Run(name, func(t *testing.T) {
			sandboxHome(t)
			writeKiroBundle(t, "sid1", map[string]any{"cwd": "/x"}, map[string]string{".lock": tc.lock})
			got := KiroAgent{}.Scan()
			if len(got) != 1 {
				t.Fatalf("scan: got %d sessions, want 1", len(got))
			}
			if got[0].Locked != tc.want {
				t.Errorf("locked: got %v, want %v", got[0].Locked, tc.want)
			}
		})
	}
}

func TestKiroNoLockFileIsUnlocked(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/x"}, nil)
	if (KiroAgent{}).Scan()[0].Locked {
		t.Error("locked: got true with no .lock file")
	}
}

// ADR-0003 D2: move rewrites the .json's cwd in place, keeping every other field.
func TestKiroRelocateMoveRewritesCwdInPlace(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1",
		map[string]any{"cwd": "/home/u/ideas/foo", "title": "keep me", "created_at": "2026-01-01"},
		map[string]string{".jsonl": kiroEvent("Prompt") + "\n"})

	s := KiroAgent{}.Scan()[0]
	newID, err := KiroAgent{}.Relocate(s, "/home/u/repos/foo", false)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if newID != "sid1" {
		t.Errorf("move must keep the id: got %q", newID)
	}

	meta := readKiroJSON(t, filepath.Join(kiroSessionsDir(), "sid1.json"))
	if meta["cwd"] != "/home/u/repos/foo" {
		t.Errorf("cwd: got %v, want the new path", meta["cwd"])
	}
	if meta["title"] != "keep me" || meta["created_at"] != "2026-01-01" {
		t.Errorf("move clobbered other fields: %+v", meta)
	}
	if meta["session_id"] != "sid1" {
		t.Errorf("session_id: got %v, want sid1", meta["session_id"])
	}
	if got := (KiroAgent{}).Scan(); len(got) != 1 || got[0].Cwd != "/home/u/repos/foo" {
		t.Errorf("after move, scan reports %+v", got)
	}
}

// ADR-0003 D1/D2: copy duplicates the whole bundle under a fresh id, preserving
// each file's extension, and leaves the original untouched.
func TestKiroRelocateCopyDuplicatesBundleUnderFreshID(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1",
		map[string]any{"cwd": "/home/u/ideas/foo", "title": "t"},
		map[string]string{".jsonl": kiroEvent("Prompt") + "\n", ".history": "ls\n"})

	s := KiroAgent{}.Scan()[0]
	newID, err := KiroAgent{}.Relocate(s, "/home/u/repos/foo", true)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if newID == "sid1" || newID == "" {
		t.Fatalf("copy must use a fresh id, got %q", newID)
	}

	// The copy's whole bundle exists under the new id.
	for _, suffix := range []string{".json", ".jsonl", ".history"} {
		if _, err := os.Stat(filepath.Join(kiroSessionsDir(), newID+suffix)); err != nil {
			t.Errorf("copy is missing %s: %v", suffix, err)
		}
	}
	// The original keeps its id and its old cwd.
	orig := readKiroJSON(t, filepath.Join(kiroSessionsDir(), "sid1.json"))
	if orig["cwd"] != "/home/u/ideas/foo" {
		t.Errorf("copy modified the original's cwd: %v", orig["cwd"])
	}
	// The copy points at the new cwd under the new id.
	cp := readKiroJSON(t, filepath.Join(kiroSessionsDir(), newID+".json"))
	if cp["cwd"] != "/home/u/repos/foo" || cp["session_id"] != newID {
		t.Errorf("copy metadata wrong: %+v", cp)
	}

	if got := (KiroAgent{}).Scan(); len(got) != 2 {
		t.Errorf("after copy: got %d sessions, want 2", len(got))
	}
}

func readKiroJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestKiroDeleteRemovesWholeBundle(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/x"},
		map[string]string{".jsonl": kiroEvent("Prompt") + "\n", ".history": "ls\n"})

	s := KiroAgent{}.Scan()[0]
	if err := (KiroAgent{}).Delete(s); err != nil {
		t.Fatalf("delete: %v", err)
	}
	entries, err := os.ReadDir(kiroSessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("delete left %d files behind", len(entries))
	}
}

func TestKiroInstalledRequiresSessionsDir(t *testing.T) {
	sandboxHome(t)
	if (KiroAgent{}).Installed() {
		t.Error("Installed() is true with no ~/.kiro/sessions/cli")
	}
	if err := os.MkdirAll(kiroSessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if !(KiroAgent{}).Installed() {
		t.Error("Installed() is false with the sessions dir present")
	}
}

// cleanTitle exists because Kiro sometimes stores a JSON blob where a title
// belongs; the list must never show JSON soup.
func TestCleanTitle(t *testing.T) {
	sid := "abcdef1234567890"
	placeholder := "(untitled · abcdef12)"

	for name, tc := range map[string]struct {
		raw  *string
		want string
	}{
		"nil":                {nil, placeholder},
		"empty":              {strp("   "), placeholder},
		"plain":              {strp("  A normal title  "), "A normal title"},
		"content array":      {strp(`{"content":[{"type":"text","text":"Real title"}]}`), "Real title"},
		"untrusted marker":   {strp("<untrusted_content_9f2>Actual question"), "Actual question"},
		"marker in blob":     {strp(`{"content":[{"type":"text","text":"<untrusted_content_1>Q"}]}`), "Q"},
		"unusable json":      {strp(`{"role":"user"}`), placeholder},
		"unusable array":     {strp(`[1,2,3]`), placeholder},
		"collapses newlines": {strp("line one\nline two\r\nline three"), "line one line two line three"},
	} {
		if got := cleanTitle(tc.raw, sid); got != tc.want {
			t.Errorf("%s: cleanTitle = %q, want %q", name, got, tc.want)
		}
	}
}

// Long titles are clamped by rune (not byte), so CJK isn't cut mid-character.
func TestCleanTitleClampsByRune(t *testing.T) {
	long := strings.Repeat("测", 200)
	got := cleanTitle(&long, "sid")
	if n := len([]rune(got)); n != 81 { // 80 runes + the ellipsis
		t.Errorf("clamped length: got %d runes, want 81", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected an ellipsis suffix, got %q", got)
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Error("clamped title is not a prefix of the source")
	}
}

// A short session id is used whole in the placeholder rather than being sliced.
func TestCleanTitlePlaceholderWithShortID(t *testing.T) {
	if got := cleanTitle(nil, "abc"); got != "(untitled · abc)" {
		t.Errorf("got %q", got)
	}
}

func strp(s string) *string { return &s }

// Rename writes into the .json Kiro itself reads, so Kiro's own listing shows it.
func TestKiroRenameWritesTheJSONAndKeepsOtherFields(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1",
		map[string]any{"cwd": "/proj", "title": "auto title", "created_at": "2026-01-01",
			"session_state": map[string]any{"keep": "me"}},
		map[string]string{".jsonl": kiroEvent("Prompt") + "\n"})

	s := (KiroAgent{}).Scan()[0]
	if err := (KiroAgent{}).Rename(s, "搬仓到内网 gitlab"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	meta := readKiroJSON(t, filepath.Join(kiroSessionsDir(), "sid1.json"))
	if meta["title"] != "搬仓到内网 gitlab" {
		t.Errorf("title: got %v", meta["title"])
	}
	if meta["cwd"] != "/proj" || meta["created_at"] != "2026-01-01" || meta["session_state"] == nil {
		t.Errorf("rename clobbered other fields: %+v", meta)
	}
	if got := (KiroAgent{}).Scan()[0].Title; got != "搬仓到内网 gitlab" {
		t.Errorf("scan still reports %q", got)
	}
}

// countingReader records how many bytes were actually pulled from the source.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The 17.7x speedup in Kiro's scan (494ms → 27.9ms) comes from stopping the
// decode once cwd and title are in hand, instead of reading the whole .json —
// which carries `session_state`, the entire conversation, averaging 274 KB and
// reaching 2.7 MB in real data.
//
// This asserts bytes consumed rather than elapsed time: timing is flaky, and the
// property that matters is "does not read the big value", not "is fast today". If
// Kiro ever emits session_state before cwd/title, correctness holds but the
// speedup is silently lost — this is what would notice.
func TestKiroDecodeMetaStopsBeforeTheConversation(t *testing.T) {
	bulk := strings.Repeat("x", 400_000)
	doc := `{"session_id":"sid1","cwd":"/home/u/proj","title":"a title",` +
		`"created_at":"2026-01-01","session_state":{"blob":"` + bulk + `"}}`

	cr := &countingReader{r: strings.NewReader(doc)}
	meta, ok := kiroDecodeMeta(cr)
	if !ok {
		t.Fatal("decode failed")
	}
	if meta.cwd != "/home/u/proj" || meta.title == nil || *meta.title != "a title" {
		t.Fatalf("wrong metadata: cwd=%q title=%v", meta.cwd, meta.title)
	}

	// The decoder buffers, so this is not a byte-exact bound — but reading a small
	// multiple of the buffer instead of 400 KB is the difference being asserted.
	const generousLimit = 64 * 1024
	if cr.n > generousLimit {
		t.Errorf("read %d bytes of a %d-byte document; expected to stop early (limit %d).\n"+
			"Kiro may have changed its key order so session_state now precedes cwd/title — "+
			"still correct, but the scan speedup is gone.", cr.n, len(doc), generousLimit)
	}
	t.Logf("read %d of %d bytes (%.1f%%)", cr.n, len(doc), 100*float64(cr.n)/float64(len(doc)))
}

// The reverse layout: correctness must survive it even though speed does not.
func TestKiroDecodeMetaStillCorrectWhenBulkComesFirst(t *testing.T) {
	bulk := strings.Repeat("y", 50_000)
	doc := `{"session_state":{"blob":"` + bulk + `"},"cwd":"/late/cwd","title":"late title"}`

	meta, ok := kiroDecodeMeta(strings.NewReader(doc))
	if !ok {
		t.Fatal("decode failed")
	}
	if meta.cwd != "/late/cwd" {
		t.Errorf("cwd: got %q, want /late/cwd", meta.cwd)
	}
	if meta.title == nil || *meta.title != "late title" {
		t.Errorf("title: got %v", meta.title)
	}
}
