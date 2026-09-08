package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These exec the real binary rather than calling into the package, because what
// they assert is the process-level contract a caller depends on: exit codes,
// which stream output goes to, and the JSON shape. That contract cannot be
// observed from inside the process.

// buildBinary compiles asbutler once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "asbutler")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// sandbox points every agent's discovery at empty temp dirs and returns the env,
// so no test can see or touch real session data.
func sandbox(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home,
		"CODEX_HOME="+filepath.Join(home, "nope"),
		"HERMES_HOME="+filepath.Join(home, "nope"),
	)
}

type result struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, bin string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return result{out.String(), errb.String(), code}
}

// A clean listing exits 0 with JSON on stdout and nothing on stderr — the shape
// the downstream consumer parses.
func TestListEmptyIsSuccessfulJSON(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	r := run(t, bin, env, "list", "--path", t.TempDir())

	if r.code != 0 {
		t.Errorf("exit: got %d, want 0 (stderr: %s)", r.code, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr should be empty on success, got %q", r.stderr)
	}
	var doc struct {
		Summary  map[string]any   `json:"summary"`
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	// An empty directory is an empty result, not an error.
	if doc.Sessions == nil {
		t.Error("sessions should be [] rather than absent")
	}
}

// Usage errors exit 2 and say so on stderr, keeping stdout clean for data.
func TestUsageErrorsExitTwo(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	for name, args := range map[string][]string{
		"unknown command":      {"frobnicate"},
		"unknown list flag":    {"list", "--paths", "/tmp"},
		"--all with --path":    {"list", "--all", "--path", "/tmp"},
		"--path without value": {"list", "--path"},
		"mv without a target":  {"mv", "someid"},
		"cp without a target":  {"cp", "someid"},
		"rename without title": {"rename", "someid"},
		"rm without ids":       {"rm"},
	} {
		t.Run(name, func(t *testing.T) {
			r := run(t, bin, env, args...)
			if r.code != 2 {
				t.Errorf("exit: got %d, want 2 (stdout %q stderr %q)", r.code, r.stdout, r.stderr)
			}
			if r.stderr == "" {
				t.Error("a usage error should explain itself on stderr")
			}
			if strings.TrimSpace(r.stdout) != "" {
				t.Errorf("stdout should stay clean for data, got %q", r.stdout)
			}
		})
	}
}

// Operations on ids that do not exist exit 1 — distinct from a usage error, so a
// caller can tell "you asked wrong" from "it did not work".
func TestFailedOperationsExitOne(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	dest := t.TempDir()
	for name, args := range map[string][]string{
		"rm unknown id":     {"rm", "no-such-id"},
		"mv unknown id":     {"mv", "no-such-id", dest},
		"cp unknown id":     {"cp", "no-such-id", dest},
		"rename unknown id": {"rename", "no-such-id", "a title"},
	} {
		t.Run(name, func(t *testing.T) {
			r := run(t, bin, env, args...)
			if r.code != 1 {
				t.Errorf("exit: got %d, want 1 (stderr %q)", r.code, r.stderr)
			}
		})
	}
}

// rm reports per id and fails the process if any one failed, so a partial batch
// is both visible in the payload and detectable from the exit code.
func TestRemoveReportsPerIDAndFailsIfAnyFailed(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	r := run(t, bin, env, "rm", "id-one", "id-two")

	if r.code != 1 {
		t.Errorf("exit: got %d, want 1", r.code)
	}
	var results []struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &results); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, r.stdout)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want one per id", len(results))
	}
	for _, res := range results {
		if res.Deleted || res.Error == "" {
			t.Errorf("%s: expected a failure with a reason, got %+v", res.ID, res)
		}
	}
}

// mv/cp take any number of ids with the destination last, and report per id like
// rm — the array shape matters to a caller, not just the exit code.
func TestRelocateReportsPerID(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	dest := t.TempDir()
	for _, verb := range []string{"mv", "cp"} {
		t.Run(verb, func(t *testing.T) {
			r := run(t, bin, env, verb, "id-one", "id-two", "id-three", dest)
			if r.code != 1 {
				t.Errorf("exit: got %d, want 1", r.code)
			}
			var results []struct {
				ID     string `json:"id"`
				NewCwd string `json:"newCwd"`
				Copied bool   `json:"copied"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &results); err != nil {
				t.Fatalf("stdout is not a JSON array: %v\n%s", err, r.stdout)
			}
			if len(results) != 3 {
				t.Fatalf("got %d results, want one per id", len(results))
			}
			if want := verb == "cp"; results[0].Copied != want {
				t.Errorf("copied: got %v, want %v for %s", results[0].Copied, want, verb)
			}
		})
	}
}

// A target that is not an existing directory is refused before anything is
// touched, since storing it would quietly orphan the session.
func TestRelocateRejectsABadTarget(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	r := run(t, bin, env, "mv", "some-id", missing)

	if r.code == 0 {
		t.Fatal("expected a non-zero exit for a missing target directory")
	}
	if !strings.Contains(r.stdout+r.stderr, "does not exist") {
		t.Errorf("the error should say the directory is missing, got stdout=%q stderr=%q", r.stdout, r.stderr)
	}
}

// -H switches to text for humans; JSON stays the default for machines.
func TestHumanFlagSwitchesFormat(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	dir := t.TempDir()

	if got := run(t, bin, env, "list", "--path", dir).stdout; !strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Errorf("default output should be JSON, got %q", got)
	}
	human := run(t, bin, env, "list", "--path", dir, "-H").stdout
	if strings.HasPrefix(strings.TrimSpace(human), "{") {
		t.Errorf("-H output should not be JSON, got %q", human)
	}
}

func TestVersionAndHelp(t *testing.T) {
	bin, env := buildBinary(t), sandbox(t)
	for _, args := range [][]string{{"version"}, {"--version"}} {
		r := run(t, bin, env, args...)
		if r.code != 0 {
			t.Errorf("%v: exit %d", args, r.code)
		}
		if !strings.Contains(r.stdout, version) {
			t.Errorf("%v: stdout %q should contain the version %q", args, r.stdout, version)
		}
	}
	// help goes to stdout and succeeds; an unknown command prints usage to stderr
	// and fails, so a caller is never left guessing which happened.
	if r := run(t, bin, env, "help"); r.code != 0 || !strings.Contains(r.stdout, "Usage:") {
		t.Errorf("help: exit %d, stdout %q", r.code, r.stdout)
	}
	if r := run(t, bin, env, "nonsense"); r.code != 2 || !strings.Contains(r.stderr, "Usage:") {
		t.Errorf("unknown command: exit %d, stderr %q", r.code, r.stderr)
	}
}
