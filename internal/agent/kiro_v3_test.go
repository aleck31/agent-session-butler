package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// writeV3 lays out one v3 session the way kiro-cli 2.33 does: sessions/<hash>/<id>/{session.json,messages.jsonl}.
func writeV3(t *testing.T, home, hash, id, cwd, title string, rows ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".kiro", "sessions", hash, id)
	if err := os.MkdirAll(filepath.Join(dir, "tool-outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"schemaVersion":"1.0.0","id":"` + id + `","title":"` + title + `","workspacePaths":["` + cwd + `"]}`
	writeFile(t, filepath.Join(dir, "session.json"), meta)
	writeFile(t, filepath.Join(dir, "messages.jsonl"), strings.Join(rows, "\n")+"\n")
	writeFile(t, filepath.Join(dir, "tool-outputs", "big.txt"), strings.Repeat("x", 1000))
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func row(ts, payload string) string {
	return `{"id":"r","timestamp":"` + ts + `","payload":` + payload + `}`
}

var v3Rows = []string{
	row("2026-10-22T03:00:00+00:00", `{"type":"user","content":"list the skills","source":"chat"}`),
	row("2026-10-22T03:00:05+00:00", `{"type":"assistant","content":"Looking."}`),
	row("2026-10-22T03:00:06+00:00", `{"type":"tool_call","toolCallId":"t1","toolName":"execute_bash","args":{"command":"ls"}}`),
	row("2026-10-22T03:00:07+00:00", `{"type":"tool_result","toolCallId":"t1","content":"a\nb\n","success":true}`),
	row("2026-10-22T03:00:08+00:00", `{"type":"assistant","content":"Two skills."}`),
	row("2026-10-22T03:00:09+00:00", `{"type":"turn_end","stopReason":"end_turn"}`),
	row("2026-10-22T03:01:00+00:00", `{"type":"user","content":"thanks","documents":[]}`), // no source: still a person
}

func v3Sandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// Both id forms, a non-session .index, and the v2 cli/ dir sharing the parent: only the two sessions count.
func TestKiroV3Scan(t *testing.T) {
	home := v3Sandbox(t)
	writeV3(t, home, "b5eb362c6081c907", "cli_bf4c16a8-ed4d-44ef-ba96-44f3104d26f7_Cqt24wzJ", "/p/ai-stack", "derived", v3Rows...)
	writeV3(t, home, "b5eb362c6081c907", "sess_77cf9fdb-1bb6-4e86-a2fb-66cf97d4c675", "/p/ai-stack", "fresh")
	if err := os.MkdirAll(filepath.Join(home, ".kiro", "sessions", "b5eb362c6081c907", ".index"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".kiro", "sessions", "cli"), 0o700); err != nil {
		t.Fatal(err)
	}

	got := map[string]Session{}
	for _, s := range (KiroAgent{}).scanV3() {
		got[s.ID] = s
	}
	if len(got) != 2 {
		t.Fatalf("scanned %d v3 sessions, want 2: %v", len(got), got)
	}
	s := got["cli_bf4c16a8-ed4d-44ef-ba96-44f3104d26f7_Cqt24wzJ"]
	if s.Store != "v3" || s.Cwd != "/p/ai-stack" || s.Title != "derived" {
		t.Errorf("store=%q cwd=%q title=%q", s.Store, s.Cwd, s.Title)
	}
	if s.FileSize < 1000 {
		t.Errorf("size %d leaves out tool-outputs/, which is disk the session occupies", s.FileSize)
	}
	if s.Locked {
		t.Error("locked without a .lock")
	}
}

func TestKiroV3LockFollowsThePid(t *testing.T) {
	home := v3Sandbox(t)
	dir := writeV3(t, home, "h", "sess_x", "/p", "t")
	writeFile(t, filepath.Join(dir, ".lock"), `{"pid":`+strconv.Itoa(os.Getpid())+`,"started_at":"2026-10-22T03:41:38Z"}`)
	if s, _ := (KiroAgent{}).parseV3(dir); !s.Locked {
		t.Error("a live pid in .lock should lock the session")
	}
	writeFile(t, filepath.Join(dir, ".lock"), `{"pid":999999999}`)
	if s, _ := (KiroAgent{}).parseV3(dir); s.Locked {
		t.Error("a dead pid in .lock should not lock the session")
	}
}

// User and assistant rows only; a row still being written is not counted.
func TestKiroV3EnrichCount(t *testing.T) {
	home := v3Sandbox(t)
	rows := append(slices.Clone(v3Rows), `{"id":"r","payload":{"type":"user","content":"still bei`)
	s, _ := (KiroAgent{}).parseV3(writeV3(t, home, "h", "sess_x", "/p", "t", rows...))
	if got := (KiroAgent{}).Enrich(s); got.MessageCount == nil || *got.MessageCount != 4 {
		t.Errorf("count = %v, want 4", got.MessageCount)
	}
}

func TestKiroV3Transcript(t *testing.T) {
	home := v3Sandbox(t)
	s, _ := (KiroAgent{}).parseV3(writeV3(t, home, "h", "sess_x", "/p", "t", v3Rows...))
	turns, err := (KiroAgent{}).Transcript(s, TranscriptOptions{Bodies: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("%d turns, want 2", len(turns))
	}
	msgs := turns[0].Messages
	if len(msgs) != 3 || msgs[0].Text != "list the skills" || msgs[0].At == nil {
		t.Fatalf("first turn = %+v", msgs)
	}
	tools := msgs[1].Tools
	if len(tools) != 1 || tools[0].Name != "execute_bash" || tools[0].OutputBytes != 4 || !strings.Contains(tools[0].Output, "a") {
		t.Errorf("tool call not attached with its result: %+v", tools)
	}
}

// Kiro lists v3 titles from .index/index.json and does not notice an edited session.json, so a
// rename that left the index would show only in asbutler.
func TestKiroV3RenameDropsTheIndex(t *testing.T) {
	home := v3Sandbox(t)
	dir := writeV3(t, home, "h", "sess_x", "/p", "old")
	index := filepath.Join(filepath.Dir(dir), ".index", "index.json")
	if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, index, `{"entries":[{"id":"sess_x","title":"old"}]}`)
	s, _ := (KiroAgent{}).parseV3(dir)
	if err := (KiroAgent{}).Rename(s, "new"); err != nil {
		t.Fatal(err)
	}
	if again, _ := (KiroAgent{}).parseV3(dir); again.Title != "new" {
		t.Errorf("title after rename = %q", again.Title)
	}
	if fileExists(index) {
		t.Error("index still holds the old title")
	}
}

// kiro-cli prints "Deleted" even for an id it does not have, so success is judged by the directory.
func TestKiroV3DeleteChecksTheDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake kiro-cli is a shell script")
	}
	home := v3Sandbox(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	args := filepath.Join(home, "args")
	dir := writeV3(t, home, "h", "sess_x", "/p", "t")
	s, _ := (KiroAgent{}).parseV3(dir)

	toolPathOnce.Clear()
	writeExe(t, filepath.Join(bin, "kiro-cli"), "#!/bin/sh\necho \"$@\" > "+args+"\necho 'Deleted chat session'\n")
	if err := (KiroAgent{}).Delete(s); err == nil || !strings.Contains(err.Error(), "still there") {
		t.Errorf("a no-op kiro-cli was taken at its word: %v", err)
	}
	if got, _ := os.ReadFile(args); strings.TrimSpace(string(got)) != "chat --delete-session sess_x --session-source v3" {
		t.Errorf("kiro-cli got %q", got)
	}

	toolPathOnce.Clear()
	writeExe(t, filepath.Join(bin, "kiro-cli"), "#!/bin/sh\nrm -rf "+dir+"\necho 'Deleted chat session'\n")
	if err := (KiroAgent{}).Delete(s); err != nil {
		t.Errorf("delete: %v", err)
	}
}

func TestKiroV3RelocateIsRefusedAndResumeNeedsNoEngine(t *testing.T) {
	s := Session{ID: "sess_x", Store: kiroStoreV3}
	if _, err := (KiroAgent{}).Relocate(s, "/q", false); !errors.Is(err, ErrRelocateUnsupported) {
		t.Errorf("relocate: %v", err)
	}
	if got := (KiroAgent{}).ResumeArgv(s, false); !slices.Equal(got, []string{"kiro-cli", "chat", "--resume-id", "sess_x"}) {
		t.Errorf("resume = %v", got)
	}
}

// A machine that has only ever run the v3 engine has no sessions/cli/ and no v1 database.
func TestKiroInstalledWithOnlyTheV3Store(t *testing.T) {
	home := v3Sandbox(t)
	writeV3(t, home, "h", "sess_x", "/p", "t")
	if !(KiroAgent{}).Installed() {
		t.Error("Kiro not detected from a v3-only store")
	}
}
