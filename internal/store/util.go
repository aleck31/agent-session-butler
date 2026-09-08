package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// lastPathComponent returns the final path element, handling both / and \
// separators and trailing slashes.
func lastPathComponent(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ExpandPath turns a user-supplied path into the absolute, cleaned form. It
// expands a leading ~ but deliberately does not resolve symlinks — callers that
// need a canonical form for comparison do that themselves, while a path being
// stored should keep the shape the user meant.
func ExpandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// resolveTargetCwd validates and normalises a relocation target. A session's cwd
// is what lets an agent resume it there and what this tool groups on, so a
// relative path, an unexpanded ~, or a directory that isn't there would all
// produce a session no directory query can reach — silently, as one more orphan.
// The realistic failure is a typo, which this turns into an error instead.
func resolveTargetCwd(newCwd string) (string, error) {
	if strings.TrimSpace(newCwd) == "" {
		return "", fmt.Errorf("target cwd must not be empty")
	}
	abs, err := ExpandPath(strings.TrimSpace(newCwd))
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", newCwd, err)
	}
	info, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("target directory does not exist: %s (create it first)", abs)
	}
	if err != nil {
		return "", fmt.Errorf("cannot inspect %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("target is not a directory: %s", abs)
	}
	return abs, nil
}

// HumanSize renders a byte count in binary units (KiB/MiB/…), like a file
// manager. Kept here so callers don't reach into internal formatting.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// SamePath reports whether a group's cwd is the directory the user asked for.
// The comparison is literal — only case-folded where the filesystem is. Symlinks
// are deliberately *not* resolved: two spellings of one directory are two session
// lists, because that is how the agents key them. Resolving both sides merged them,
// so a query for either spelling returned the union of both.
func SamePath(groupCwd, want string) bool {
	return pathEqual(filepath.Clean(groupCwd), want)
}

func pathEqual(a, b string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
