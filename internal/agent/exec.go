package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Agents are mutated through their own CLI. asbutler is often started by something other than the
// user's shell — a GUI app, `ssh host cmd` — and so lacks the PATH the shell's rc files build.
// Lookup order: ASBUTLER_<TOOL> override, inherited PATH, known install dirs, the login shell's PATH.

// toolTimeout bounds a delegated CLI call; without it a hung tool is a consumer's endless spinner.
const toolTimeout = 60 * time.Second

// toolTimeoutFor is the value actually used, so a test can shorten it.
var toolTimeoutFor = toolTimeout

var toolPathOnce sync.Map // tool name → resolved path; misses are not cached

// loginPath asks the user's shell for its PATH, at most once and only after the cheaper steps miss.
var loginPath = sync.OnceValue(shellPath)

// extraToolDirs are the install locations to try when PATH does not have the tool.
func extraToolDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, ".toolbox", "bin"), // Amazon's toolbox
			filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, "go", "bin"),
		)
	}
	switch runtime.GOOS {
	case "darwin":
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	case "linux":
		dirs = append(dirs, "/usr/local/bin", "/snap/bin")
	}
	return dirs
}

// toolPath resolves an agent CLI to an absolute path, or explains where it looked.
func toolPath(name string) (string, error) {
	if p, ok := toolPathOnce.Load(name); ok {
		return p.(string), nil
	}
	override := "ASBUTLER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if p := os.Getenv(override); p != "" {
		if !isExecutable(p) {
			return "", fmt.Errorf("%s=%s is not an executable file", override, p)
		}
		toolPathOnce.Store(name, p)
		return p, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		if p = lookIn(extraToolDirs(), name); p == "" {
			p = lookIn(filepath.SplitList(loginPath()), name)
		}
	}
	if p == "" {
		return "", fmt.Errorf("%s not found on PATH, in %s, or on your login shell's PATH; set %s to its location",
			name, strings.Join(extraToolDirs(), ", "), override)
	}
	toolPathOnce.Store(name, p)
	return p, nil
}

func lookIn(dirs []string, name string) string {
	for _, dir := range dirs {
		if p := filepath.Join(dir, name); dir != "" && isExecutable(p) {
			return p
		}
	}
	return ""
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

// shellPath runs the user's shell as an interactive login shell, so both profile and rc files apply,
// and reads PATH from `env` — an external command, so this works the same in fish.
func shellPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" || runtime.GOOS == "windows" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-ilc", "env")
	killGroupOnCancel(cmd)
	out, _ := cmd.Output() // rc files may fail or print noise; only the PATH= line matters
	path := ""
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "PATH="); ok {
			path = v
		}
	}
	return path
}

// toolCommand prepares a run of an agent CLI. The tool's own directories go first on the child's
// PATH, so a `#!/usr/bin/env node` script finds the interpreter installed beside it.
func toolCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	p, err := toolPath(name)
	if err != nil {
		return nil, err
	}
	dirs := []string{filepath.Dir(p)}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		dirs = append(dirs, filepath.Dir(real))
	}
	cmd := exec.CommandContext(ctx, p, args...)
	cmd.Env = append(os.Environ(), "PATH="+strings.Join(append(dirs, os.Getenv("PATH")), string(os.PathListSeparator)))
	killGroupOnCancel(cmd)
	return cmd, nil
}

// runTool executes an agent's CLI and returns its combined output.
func runTool(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeoutFor)
	defer cancel()
	cmd, err := toolCommand(ctx, name, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s did not finish within %s", name, toolTimeoutFor)
	}
	// The tool exited cleanly but left a background child holding its output; its result stands.
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	return string(out), err
}
