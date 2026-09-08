package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reported failure: an editor plugin spawned asbutler, which then could not
// find kiro-cli because a GUI-launched process inherits the session PATH rather
// than the one a shell rc file builds — so ~/.local/bin is absent.
func TestToolPathLooksBeyondPATH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "/nonexistent-for-this-test")
	toolPathOnce.Clear()

	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "faketool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := toolPath("faketool")
	if err != nil {
		t.Fatalf("a tool in ~/.local/bin was not found: %v", err)
	}
	if got != exe {
		t.Errorf("got %q, want %q", got, exe)
	}
}

// A tool that is genuinely absent must say where it looked. "not found in $PATH"
// was the original message and it sent the reporter down the wrong path.
func TestToolPathErrorNamesWhereItLooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "/nonexistent-for-this-test")
	toolPathOnce.Clear()

	_, err := toolPath("definitely-not-installed")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "definitely-not-installed") {
		t.Errorf("error should name the tool: %q", msg)
	}
	if !strings.Contains(msg, ".local/bin") {
		t.Errorf("error should list the directories searched: %q", msg)
	}
}

// A non-executable file with the right name is not the tool.
func TestToolPathIgnoresNonExecutables(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "/nonexistent-for-this-test")
	toolPathOnce.Clear()

	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "notexec"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := toolPath("notexec"); err == nil {
		t.Errorf("a non-executable was accepted: %s", p)
	}
}

// A hung tool must not hang asbutler: a caller like an editor plugin would show a
// spinner that never stops.
func TestRunToolTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("takes as long as toolTimeout")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "/nonexistent-for-this-test")
	toolPathOnce.Clear()

	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// Sleeps well past the timeout.
	// An absolute path: PATH is deliberately broken in this test.
	sleepBin := "/bin/sleep"
	script := "#!/bin/sh\nexec " + sleepBin + " 600\n"
	if err := os.WriteFile(filepath.Join(bin, "hangs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Shorten the wait for the test rather than sitting here for a minute.
	orig := toolTimeoutFor
	toolTimeoutFor = 300 * time.Millisecond
	t.Cleanup(func() { toolTimeoutFor = orig })

	_, err := runTool("hangs")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("error should say it timed out, got %q", err)
	}
}
