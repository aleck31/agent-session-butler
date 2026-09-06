package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sandboxHome points os.UserHomeDir() at a temp dir so tests never touch the
// real ~/.claude or ~/.kiro (ADR-0003 D5).
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	return home
}

// writeClaudeSession creates ~/.claude/projects/<dirName>/<file>.jsonl with the
// given raw lines. dirName is passed literally so a test can force two distinct
// cwds into one encoded directory.
func writeClaudeSession(t *testing.T, dirName, file string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(claudeProjectsRoot(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file+".jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func userLine(sessionID, cwd, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "sessionId": sessionID, "cwd": cwd,
		"message": map[string]any{"content": text},
	})
	return string(b)
}

func assistantLine(sessionID, cwd, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "sessionId": sessionID, "cwd": cwd,
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})
	return string(b)
}

// ADR-0001 D1: the encoded project-dir name is lossy — /a/b-c and /a/b/c both
// encode to "-a-b-c". cwd MUST come from the file contents, so two sessions
// sharing one encoded directory still report their own distinct cwd.
func TestClaudeScanReadsCwdFromContentsNotDirName(t *testing.T) {
	sandboxHome(t)
	// Both of these encode to the same directory name.
	cwdA := "/home/u/ideas/agent-session-butler"
	cwdB := "/home/u/ideas/agent/session/butler"
	encoded := claudeEncodeCwd(cwdA)
	if claudeEncodeCwd(cwdB) != encoded {
		t.Fatalf("premise broken: %q and %q no longer collide", cwdA, cwdB)
	}

	writeClaudeSession(t, encoded, "aaa", userLine("aaa", cwdA, "hi from A"))
	writeClaudeSession(t, encoded, "bbb", userLine("bbb", cwdB, "hi from B"))

	got := ClaudeCodeAgent{}.Scan()
	if len(got) != 2 {
		t.Fatalf("scan: got %d sessions, want 2", len(got))
	}
	byID := map[string]string{}
	for _, s := range got {
		byID[s.ID] = s.Cwd
	}
	if byID["aaa"] != cwdA {
		t.Errorf("session aaa cwd: got %q, want %q", byID["aaa"], cwdA)
	}
	if byID["bbb"] != cwdB {
		t.Errorf("session bbb cwd: got %q, want %q", byID["bbb"], cwdB)
	}
}

// ADR-0001 D2: files with only ai-title/agent-name rows are sidecars, not
// sessions — they have no cwd and no messages, and must not be listed.
func TestClaudeScanSkipsMetadataOnlyAndEmptyFiles(t *testing.T) {
	sandboxHome(t)
	dir := claudeEncodeCwd("/home/u/proj")

	writeClaudeSession(t, dir, "meta",
		`{"type":"ai-title","aiTitle":"Some title","sessionId":"meta"}`,
		`{"type":"agent-name","agentName":"claude","sessionId":"meta"}`)
	writeClaudeSession(t, dir, "empty")
	writeClaudeSession(t, dir, "real", userLine("real", "/home/u/proj", "actual question"))

	got := ClaudeCodeAgent{}.Scan()
	if len(got) != 1 {
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		t.Fatalf("scan: got %v, want only the real session", ids)
	}
	if got[0].ID != "real" {
		t.Errorf("scan: got %q, want %q", got[0].ID, "real")
	}
}

// A real conversation with no cwd recorded lands in "(unknown)" rather than
// being dropped — it has messages, so it's a session (ADR-0001 D8).
func TestClaudeConversationWithoutCwdBecomesUnknown(t *testing.T) {
	sandboxHome(t)
	writeClaudeSession(t, "some-dir", "nocwd",
		`{"type":"user","sessionId":"nocwd","message":{"content":"hi"}}`)

	got := ClaudeCodeAgent{}.Scan()
	if len(got) != 1 {
		t.Fatalf("scan: got %d sessions, want 1", len(got))
	}
	if got[0].Cwd != "(unknown)" {
		t.Errorf("cwd: got %q, want %q", got[0].Cwd, "(unknown)")
	}
}

// Non-.jsonl files and subdirectories (memory/, etc.) are not sessions.
func TestClaudeScanIgnoresNonJsonlAndSubdirs(t *testing.T) {
	sandboxHome(t)
	dir := filepath.Join(claudeProjectsRoot(), "proj")
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeClaudeSession(t, "proj", "real", userLine("real", "/x", "q"))

	if got := (ClaudeCodeAgent{}).Scan(); len(got) != 1 {
		t.Fatalf("scan: got %d sessions, want 1", len(got))
	}
}

// Enrich counts user+assistant turns only, and prefers an ai-title over the
// first user message.
func TestClaudeEnrichCountsAndPrefersAITitle(t *testing.T) {
	sandboxHome(t)
	writeClaudeSession(t, "proj", "s1",
		userLine("s1", "/x", "first question"),
		assistantLine("s1", "/x", "an answer"),
		userLine("s1", "/x", "second question"),
		`{"type":"summary","summary":"not a message"}`,
		`{"type":"ai-title","aiTitle":"Chosen Title","sessionId":"s1"}`,
		`not json at all`)

	got := ClaudeCodeAgent{}.Scan()
	if len(got) != 1 {
		t.Fatalf("scan: got %d, want 1", len(got))
	}
	if got[0].MessageCount != nil {
		t.Error("scan must leave MessageCount nil (enriched lazily)")
	}

	e := ClaudeCodeAgent{}.Enrich(got[0])
	if e.MessageCount == nil || *e.MessageCount != 3 {
		t.Errorf("messageCount: got %v, want 3 (2 user + 1 assistant)", e.MessageCount)
	}
	if e.Title != "Chosen Title" {
		t.Errorf("title: got %q, want the ai-title", e.Title)
	}
}

// Without an ai-title, the title falls back to the first user message, clamped
// to 60 runes — counted in runes, not bytes, so CJK isn't cut mid-character.
func TestClaudeEnrichTitleFallsBackToFirstUserMessageClampedByRune(t *testing.T) {
	sandboxHome(t)
	long := strings.Repeat("测", 100)
	writeClaudeSession(t, "proj", "s1", userLine("s1", "/x", long))

	e := ClaudeCodeAgent{}.Enrich(ClaudeCodeAgent{}.Scan()[0])
	if n := len([]rune(e.Title)); n != 60 {
		t.Errorf("title length: got %d runes, want 60", n)
	}
	if !strings.HasPrefix(long, e.Title) {
		t.Errorf("title %q is not a prefix of the source text", e.Title)
	}
}

// ADR-0003 D3: relocating a Claude session must do BOTH — place the file in
// newCwd's encoded dir (how Claude finds it) and rewrite the in-file cwd on
// every line (how this tool groups it). Doing only one leaves them disagreeing.
func TestClaudeRelocateMoveRewritesBothDirAndInFileCwd(t *testing.T) {
	sandboxHome(t)
	oldCwd, newCwd := "/home/u/ideas/foo", "/home/u/repos/foo"
	src := writeClaudeSession(t, claudeEncodeCwd(oldCwd), "sid1",
		userLine("sid1", oldCwd, "q"),
		assistantLine("sid1", oldCwd, "a"),
		`{"type":"ai-title","aiTitle":"T","sessionId":"sid1"}`) // no cwd field

	s := ClaudeCodeAgent{}.Scan()[0]
	newID, err := ClaudeCodeAgent{}.Relocate(s, newCwd, false)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if newID != "sid1" {
		t.Errorf("move must keep the id: got %q, want %q", newID, "sid1")
	}

	// 1. The file now lives in newCwd's encoded project dir.
	dst := filepath.Join(claudeProjectsRoot(), claudeEncodeCwd(newCwd), "sid1.jsonl")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("moved file not at %s: %v", dst, err)
	}
	// 2. A move removes the original.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("move left the source behind at %s", src)
	}
	// 3. Every line that had a cwd now carries newCwd; lines without one are
	//    left alone rather than gaining a spurious field.
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	withCwd := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			t.Fatalf("rewrote a line into invalid JSON: %s", line)
		}
		c, ok := obj["cwd"]
		if !ok {
			continue
		}
		withCwd++
		if c != newCwd {
			t.Errorf("line cwd: got %q, want %q", c, newCwd)
		}
	}
	if withCwd != 2 {
		t.Errorf("lines carrying cwd: got %d, want 2", withCwd)
	}

	// 4. And the tool now groups it under the new cwd.
	after := ClaudeCodeAgent{}.Scan()
	if len(after) != 1 || after[0].Cwd != newCwd {
		t.Errorf("after move, scan reports %+v — want a single session at %q", after, newCwd)
	}
}

// ADR-0003 D1: copy keeps the original and creates a duplicate under a fresh id.
func TestClaudeRelocateCopyKeepsOriginalAndUsesFreshID(t *testing.T) {
	sandboxHome(t)
	oldCwd, newCwd := "/home/u/ideas/foo", "/home/u/repos/foo"
	src := writeClaudeSession(t, claudeEncodeCwd(oldCwd), "sid1", userLine("sid1", oldCwd, "q"))

	s := ClaudeCodeAgent{}.Scan()[0]
	newID, err := ClaudeCodeAgent{}.Relocate(s, newCwd, true)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if newID == "sid1" || newID == "" {
		t.Errorf("copy must use a fresh id, got %q", newID)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("copy removed the original: %v", err)
	}

	after := ClaudeCodeAgent{}.Scan()
	if len(after) != 2 {
		t.Fatalf("after copy: got %d sessions, want 2", len(after))
	}
	cwds := []string{after[0].Cwd, after[1].Cwd}
	sort.Strings(cwds)
	if cwds[0] != oldCwd || cwds[1] != newCwd {
		t.Errorf("cwds after copy: got %v, want [%s %s]", cwds, oldCwd, newCwd)
	}
}

// A failed/empty rewrite must not leave a stub behind, and (for a move) must
// never reach the point of deleting the source.
func TestClaudeRewriteCwdRefusesEmptySource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.jsonl")

	if err := claudeRewriteCwd(src, dst, "/new"); err == nil {
		t.Fatal("expected an error for a source with no lines")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a failed rewrite left a stub destination file behind")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was disturbed: %v", err)
	}
}

func TestClaudeRelocateWithoutAFileErrors(t *testing.T) {
	sandboxHome(t)
	if _, err := (ClaudeCodeAgent{}).Relocate(Session{ID: "x"}, "/new", false); err == nil {
		t.Error("expected an error relocating a session with no file")
	}
}

func TestClaudeEncodeCwd(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/home/u/proj", "-home-u-proj"},
		{"/home/u/my.proj", "-home-u-my-proj"},
		{"/home/u/a-b", "-home-u-a-b"},
	} {
		if got := claudeEncodeCwd(tc.in); got != tc.want {
			t.Errorf("claudeEncodeCwd(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClaudeInstalledRequiresProjectsDir(t *testing.T) {
	sandboxHome(t)
	if (ClaudeCodeAgent{}).Installed() {
		t.Error("Installed() is true with no ~/.claude/projects")
	}
	if err := os.MkdirAll(claudeProjectsRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	if !(ClaudeCodeAgent{}).Installed() {
		t.Error("Installed() is false with ~/.claude/projects present")
	}
}

func TestClaudeDeleteRemovesTheFile(t *testing.T) {
	sandboxHome(t)
	path := writeClaudeSession(t, "proj", "s1", userLine("s1", "/x", "q"))
	s := ClaudeCodeAgent{}.Scan()[0]
	if err := (ClaudeCodeAgent{}).Delete(s); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file still present after delete")
	}
}

func TestExtractText(t *testing.T) {
	for name, tc := range map[string]struct {
		in   any
		want string
	}{
		"string content": {map[string]any{"content": "plain"}, "plain"},
		"array content":  {map[string]any{"content": []any{map[string]any{"type": "text", "text": "part"}}}, "part"},
		"skips non-text": {map[string]any{"content": []any{
			map[string]any{"type": "thinking", "thinking": "hmm"},
			map[string]any{"type": "text", "text": "visible"},
		}}, "visible"},
		"not an object": {"just a string", ""},
		"no content":    {map[string]any{"role": "user"}, ""},
	} {
		if got := extractText(tc.in); got != tc.want {
			t.Errorf("%s: extractText = %q, want %q", name, got, tc.want)
		}
	}
}
