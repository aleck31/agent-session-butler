package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLastPathComponent(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unix":               {"/home/u/proj", "proj"},
		"trailing slash":     {"/home/u/proj/", "proj"},
		"windows":            {`C:\Users\u\proj`, "proj"},
		"windows trailing":   {`C:\Users\u\proj\`, "proj"},
		"mixed separators":   {`/home/u\proj`, "proj"},
		"no separator":       {"proj", "proj"},
		"root":               {"/", ""},
		"empty":              {"", ""},
		"dot dir":            {"/home/u/.config", ".config"},
		"placeholder cwd":    {"(unknown)", "(unknown)"},
		"dashes not touched": {"/home/u/agent-session-butler", "agent-session-butler"},
	} {
		if got := lastPathComponent(tc.in); got != tc.want {
			t.Errorf("%s: lastPathComponent(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// Binary units, like a file manager — the boundary cases are what a size column
// gets wrong in practice.
func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024*1024 - 1, "1024.0 KiB"}, // rounds up in unit but stays in KiB
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1610612736, "1.5 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
		{1 << 50, "1.0 PiB"},
		{1 << 60, "1.0 EiB"},
	} {
		if got := HumanSize(tc.in); got != tc.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolvePathExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// t.TempDir() can sit under a symlink (macOS /var → /private/var), and
	// ResolvePath resolves symlinks, so compare against the resolved home.
	wantHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	// Symlink resolution only applies to paths that exist, so create the targets.
	if err := os.MkdirAll(filepath.Join(home, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ in, want string }{
		{"~", wantHome},
		{"~/", wantHome},
		{"~/proj", filepath.Join(wantHome, "proj")},
		{"~/a/b", filepath.Join(wantHome, "a", "b")},
	} {
		got, err := ResolvePath(tc.in)
		if err != nil {
			t.Fatalf("ResolvePath(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ResolvePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// "~notme" is a literal directory name, not a home-directory reference.
func TestResolvePathLeavesBareTildePrefixAlone(t *testing.T) {
	got, err := ResolvePath("~notahome")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "~notahome" {
		t.Errorf("ResolvePath(%q) = %q, want it treated as a literal name", "~notahome", got)
	}
}

func TestResolvePathMakesRelativeAbsolute(t *testing.T) {
	got, err := ResolvePath(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolvePath(%q) = %q, want an absolute path", ".", got)
	}
}

// Resolution is best-effort: a path that no longer exists still resolves to its
// absolute form, so orphaned directories stay queryable.
func TestResolvePathKeepsNonexistentPathsQueryable(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted", "project")
	got, err := ResolvePath(gone)
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolvePath(%q) = %q, want an absolute path", gone, got)
	}
}

// An agent may have recorded an unresolved path (macOS /tmp/x vs /private/tmp/x)
// while the user asks with the other form — both must match.
func TestSamePathToleratesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	// The user asks via the symlink; the agent recorded the real path.
	want, err := ResolvePath(link)
	if err != nil {
		t.Fatal(err)
	}
	if !SamePath(resolvedReal, want) {
		t.Errorf("SamePath(%q, %q) = false, want true", resolvedReal, want)
	}
	// And the reverse: the agent recorded the symlink path.
	want2, err := ResolvePath(real)
	if err != nil {
		t.Fatal(err)
	}
	if !SamePath(link, want2) {
		t.Errorf("SamePath(%q, %q) = false, want true", link, want2)
	}
}

func TestSamePathIgnoresTrailingSlashAndDotSegments(t *testing.T) {
	dir := t.TempDir()
	want, err := ResolvePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{dir, dir + string(filepath.Separator), filepath.Join(dir, ".")} {
		if !SamePath(cwd, want) {
			t.Errorf("SamePath(%q, %q) = false, want true", cwd, want)
		}
	}
}

// Matching is on the exact directory: a parent must not pick up its children's
// sessions (the grouping model is keyed on exact cwd — ADR-0002 D2).
func TestSamePathDoesNotMatchSubtree(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	wantParent, err := ResolvePath(parent)
	if err != nil {
		t.Fatal(err)
	}
	if SamePath(child, wantParent) {
		t.Errorf("SamePath(%q, %q) = true — a parent query must not match a child", child, wantParent)
	}
}

// The placeholder cwd is not a real path and must never match a directory query.
func TestSamePathRejectsUnknownPlaceholder(t *testing.T) {
	want, err := ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if SamePath("(unknown)", want) {
		t.Error(`SamePath("(unknown)", …) = true, want false`)
	}
}

// Case-insensitive comparison only where the filesystem is: macOS and Windows.
func TestPathEqualCaseSensitivityFollowsPlatform(t *testing.T) {
	a, b := "/Home/User/Proj", "/home/user/proj"
	got := pathEqual(a, b)
	want := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if got != want {
		t.Errorf("pathEqual(%q, %q) = %v on %s, want %v", a, b, got, runtime.GOOS, want)
	}
	if !pathEqual("/same/path", "/same/path") {
		t.Error("pathEqual on identical paths = false")
	}
	if pathEqual("/a", "/b") {
		t.Error("pathEqual on different paths = true")
	}
}

// mv/cp take any number of ids with the destination last, like the shell's mv
// and like delete's multi-id form. These assertions are on the argument split,
// which is the part a caller can get wrong.
