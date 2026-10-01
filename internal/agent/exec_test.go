package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// No test may consult the developer's own shell: it is slow, and finds whatever they have installed.
func TestMain(m *testing.M) {
	loginPath = func() string { return "" }
	os.Exit(m.Run())
}

// sandboxBin is a fresh HOME whose ~/.local/bin is the only place a tool can come from.
func sandboxBin(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	toolPathOnce.Clear()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestToolPathOverride(t *testing.T) {
	sandboxBin(t)
	exe := filepath.Join(t.TempDir(), "mytool")
	writeExe(t, exe, "#!/bin/sh\n")
	t.Setenv("ASBUTLER_MY_TOOL", exe)
	if got, err := toolPath("my-tool"); err != nil || got != exe {
		t.Errorf("toolPath = %q, %v; want the override %q", got, err, exe)
	}

	// A wrong override is an error naming it, not a silent fall-through to some other copy.
	toolPathOnce.Clear()
	t.Setenv("ASBUTLER_MY_TOOL", "/nonexistent/mytool")
	if _, err := toolPath("my-tool"); err == nil || !strings.Contains(err.Error(), "ASBUTLER_MY_TOOL") {
		t.Errorf("err = %v, want one naming ASBUTLER_MY_TOOL", err)
	}
}

// A dir only the shell's rc files add — pnpm, bun, nvm — is reachable through the login shell.
func TestToolPathFallsBackToTheLoginShell(t *testing.T) {
	sandboxBin(t)
	rcDir := t.TempDir()
	writeExe(t, filepath.Join(rcDir, "rctool"), "#!/bin/sh\n")
	orig := loginPath
	loginPath = func() string { return "/nonexistent:" + rcDir }
	t.Cleanup(func() { loginPath = orig })

	if got, err := toolPath("rctool"); err != nil || got != filepath.Join(rcDir, "rctool") {
		t.Errorf("toolPath = %q, %v; want it found on the login shell's PATH", got, err)
	}
}

// rc files print banners and warnings; only the PATH line of `env` may be taken.
func TestShellPathReadsOnlyThePathLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no login shell on Windows")
	}
	shell := filepath.Join(t.TempDir(), "fakeshell")
	writeExe(t, shell, "#!/bin/sh\necho 'Welcome! PATH=/from/a/banner is not it'\necho PATH=/real/one:/real/two\necho HOME=/x\nexit 1\n")
	t.Setenv("SHELL", shell)
	if got := shellPath(); got != "/real/one:/real/two" {
		t.Errorf("shellPath = %q", got)
	}
}

// nvm installs node beside codex; a `#!/usr/bin/env node` script must find it.
func TestToolFindsAnInterpreterBesideIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shebangs are a unix mechanism")
	}
	bin := sandboxBin(t)
	writeExe(t, filepath.Join(bin, "fakenode"), "#!/bin/sh\necho ran-by-fakenode\n")
	writeExe(t, filepath.Join(bin, "scripttool"), "#!/usr/bin/env fakenode\n")
	if out, err := runTool("scripttool"); err != nil || !strings.Contains(out, "ran-by-fakenode") {
		t.Errorf("runTool = %q, %v", out, err)
	}
}

// The toolbox shim runs the real codex as a child; the timeout has to reach that child too.
func TestTimeoutReachesAGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a unix mechanism")
	}
	bin := sandboxBin(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExe(t, filepath.Join(bin, "wrapper"), "#!/bin/sh\n/bin/sleep 30 &\necho $! > "+pidFile+"\nwait\n")
	orig := toolTimeoutFor
	// Long enough for the script to start: macOS scans a new executable on first run.
	toolTimeoutFor = 2 * time.Second
	t.Cleanup(func() { toolTimeoutFor = orig })

	start := time.Now()
	if _, err := runTool("wrapper"); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("returned after %v; the grandchild kept the call alive", took)
	}
	pid, _ := os.ReadFile(pidFile)
	if p := strings.TrimSpace(string(pid)); p != "" && exec.Command("kill", "-0", p).Run() == nil {
		_ = exec.Command("kill", p).Run()
		t.Errorf("grandchild %s survived the timeout", p)
	}
}
