package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// toolTimeout bounds a delegated CLI call. These are local operations on a local
// store, so a minute is generous; without a bound a hung tool hangs asbutler,
// which for a caller like an editor plugin means a spinner that never stops.
const toolTimeout = 60 * time.Second

// toolTimeoutFor is the value actually used, so a test can shorten it.
var toolTimeoutFor = toolTimeout

// Several agents are mutated through their own CLI rather than by writing their
// stores. Finding those binaries cannot rely on PATH alone: a webui launched from
// Finder or a desktop entry inherits the session's PATH, not the one a shell rc
// file builds, so ~/.local/bin and Homebrew are typically absent. Deleting a
// session then failed with "executable file not found in $PATH".
//
// So look in PATH first, then in the places these tools actually install to.

var (
	toolPathOnce sync.Map // tool name → resolved path, resolved at most once
)

// extraToolDirs are the install locations to try when PATH does not have the tool.
func extraToolDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	dirs := []string{}
	if home != "" {
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

// toolPath resolves an agent CLI to an absolute path, or returns an error naming
// where it looked — a message a user can act on, unlike "not found in $PATH".
func toolPath(name string) (string, error) {
	if v, ok := toolPathOnce.Load(name); ok {
		if p, isStr := v.(string); isStr {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		toolPathOnce.Store(name, p)
		return p, nil
	}
	for _, dir := range extraToolDirs() {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			toolPathOnce.Store(name, p)
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not installed, or not where this could find it "+
		"(PATH, %s)", name, strings.Join(extraToolDirs(), ", "))
}

// runTool executes an agent's CLI and returns its combined output. A timeout is
// reported as such rather than as the empty output the context cancellation
// leaves behind.
func runTool(name string, args ...string) (string, error) {
	p, err := toolPath(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeoutFor)
	defer cancel()

	out, err := exec.CommandContext(ctx, p, args...).CombinedOutput()
	// Report a timeout as one: the context's error survives, while the process's
	// own status after being killed says nothing useful.
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s did not finish within %s", name, toolTimeoutFor)
	}
	return string(out), err
}
