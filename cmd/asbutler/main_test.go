package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ADR-0002 D2: `list` is a per-directory query. These are the scope rules that
// keep eager enrichment affordable — a regression here is the difference between
// a sub-second answer and a machine-wide scan.
func TestParseListArgsScopeRules(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want listOptions
	}{
		"no flags defaults to the current directory": {
			nil, listOptions{pathFilter: "."},
		},
		"--all means machine-wide (empty pathFilter)": {
			[]string{"--all"}, listOptions{},
		},
		"--path narrows": {
			[]string{"--path", "~/repos/foo"}, listOptions{pathFilter: "~/repos/foo"},
		},
		"--path= form": {
			[]string{"--path=/tmp/x"}, listOptions{pathFilter: "/tmp/x"},
		},
		"-p short form": {
			[]string{"-p", "/tmp/x"}, listOptions{pathFilter: "/tmp/x"},
		},
		"-p= form": {
			[]string{"-p=/tmp/x"}, listOptions{pathFilter: "/tmp/x"},
		},
		"-o implies --all": {
			[]string{"-o"}, listOptions{orphansOnly: true},
		},
		"--orphans implies --all": {
			[]string{"--orphans"}, listOptions{orphansOnly: true},
		},
		"-H keeps the default scope": {
			[]string{"-H"}, listOptions{human: true, pathFilter: "."},
		},
		"--human long form": {
			[]string{"--human"}, listOptions{human: true, pathFilter: "."},
		},
		"-a filters agents within the default scope": {
			[]string{"-a", "claude"}, listOptions{agentFilter: "claude", pathFilter: "."},
		},
		"--agent= form": {
			[]string{"--agent=kiro"}, listOptions{agentFilter: "kiro", pathFilter: "."},
		},
		"-a= form": {
			[]string{"-a=kiro"}, listOptions{agentFilter: "kiro", pathFilter: "."},
		},
		"--agent composes with --all": {
			[]string{"--all", "-a", "hermes"}, listOptions{agentFilter: "hermes"},
		},
		"--agent composes with --path": {
			[]string{"--path", "/x", "-a", "kiro"}, listOptions{agentFilter: "kiro", pathFilter: "/x"},
		},
		"-o composes with --agent and still implies --all": {
			[]string{"-o", "-a", "kiro"}, listOptions{orphansOnly: true, agentFilter: "kiro"},
		},
		"-o with -H": {
			[]string{"-o", "-H"}, listOptions{orphansOnly: true, human: true},
		},
		"last --path wins": {
			[]string{"--path", "/a", "--path", "/b"}, listOptions{pathFilter: "/b"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseListArgs(tc.args)
			if err != nil {
				t.Fatalf("parseListArgs(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseListArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseListArgsErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"--all and --path conflict":      {"--all", "--path", "/x"},
		"--path and --all conflict":      {"--path", "/x", "--all"},
		"--all and --path= conflict":     {"--all", "--path=/x"},
		"--path with no value":           {"--path"},
		"-p with no value":               {"-p"},
		"--agent with no value":          {"--agent"},
		"-a with no value":               {"-a"},
		"a typo'd flag is not swallowed": {"--paths", "/x"},
		"a stray positional":             {"/some/path"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseListArgs(args); err == nil {
				t.Errorf("parseListArgs(%v): expected an error", args)
			}
		})
	}
}

// -o and --all are compatible: -o already implies --all, so saying both is not a
// conflict.
func TestParseListArgsOrphansWithExplicitAll(t *testing.T) {
	got, err := parseListArgs([]string{"-o", "--all"})
	if err != nil {
		t.Fatalf("parseListArgs: %v", err)
	}
	if got.pathFilter != "" || !got.orphansOnly {
		t.Errorf("got %+v, want machine-wide orphans-only", got)
	}
}

func TestResolvePathExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// t.TempDir() can sit under a symlink (macOS /var → /private/var), and
	// resolvePath resolves symlinks, so compare against the resolved home.
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
		got, err := resolvePath(tc.in)
		if err != nil {
			t.Fatalf("resolvePath(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("resolvePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// "~notme" is a literal directory name, not a home-directory reference.
func TestResolvePathLeavesBareTildePrefixAlone(t *testing.T) {
	got, err := resolvePath("~notahome")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "~notahome" {
		t.Errorf("resolvePath(%q) = %q, want it treated as a literal name", "~notahome", got)
	}
}

func TestResolvePathMakesRelativeAbsolute(t *testing.T) {
	got, err := resolvePath(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolvePath(%q) = %q, want an absolute path", ".", got)
	}
}

// Resolution is best-effort: a path that no longer exists still resolves to its
// absolute form, so orphaned directories stay queryable.
func TestResolvePathKeepsNonexistentPathsQueryable(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted", "project")
	got, err := resolvePath(gone)
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolvePath(%q) = %q, want an absolute path", gone, got)
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
	want, err := resolvePath(link)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(resolvedReal, want) {
		t.Errorf("samePath(%q, %q) = false, want true", resolvedReal, want)
	}
	// And the reverse: the agent recorded the symlink path.
	want2, err := resolvePath(real)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(link, want2) {
		t.Errorf("samePath(%q, %q) = false, want true", link, want2)
	}
}

func TestSamePathIgnoresTrailingSlashAndDotSegments(t *testing.T) {
	dir := t.TempDir()
	want, err := resolvePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{dir, dir + string(filepath.Separator), filepath.Join(dir, ".")} {
		if !samePath(cwd, want) {
			t.Errorf("samePath(%q, %q) = false, want true", cwd, want)
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
	wantParent, err := resolvePath(parent)
	if err != nil {
		t.Fatal(err)
	}
	if samePath(child, wantParent) {
		t.Errorf("samePath(%q, %q) = true — a parent query must not match a child", child, wantParent)
	}
}

// The placeholder cwd is not a real path and must never match a directory query.
func TestSamePathRejectsUnknownPlaceholder(t *testing.T) {
	want, err := resolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if samePath("(unknown)", want) {
		t.Error(`samePath("(unknown)", …) = true, want false`)
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
func TestRelocateArgSplit(t *testing.T) {
	split := func(pos []string) (ids []string, cwd string, ok bool) {
		if len(pos) < 2 {
			return nil, "", false
		}
		return pos[:len(pos)-1], pos[len(pos)-1], true
	}

	for name, tc := range map[string]struct {
		pos     []string
		wantIDs []string
		wantCwd string
		wantOK  bool
	}{
		"single id":    {[]string{"a", "/new"}, []string{"a"}, "/new", true},
		"three ids":    {[]string{"a", "b", "c", "/new"}, []string{"a", "b", "c"}, "/new", true},
		"only a cwd":   {[]string{"/new"}, nil, "", false},
		"nothing":      {nil, nil, "", false},
		"cwd with ~":   {[]string{"a", "~/repos/x"}, []string{"a"}, "~/repos/x", true},
		"id-like cwd":  {[]string{"a", "b"}, []string{"a"}, "b", true},
		"many ids one": {[]string{"a", "b"}, []string{"a"}, "b", true},
	} {
		t.Run(name, func(t *testing.T) {
			ids, cwd, ok := split(tc.pos)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if cwd != tc.wantCwd {
				t.Errorf("cwd: got %q, want %q", cwd, tc.wantCwd)
			}
			if len(ids) != len(tc.wantIDs) {
				t.Fatalf("ids: got %v, want %v", ids, tc.wantIDs)
			}
			for i := range ids {
				if ids[i] != tc.wantIDs[i] {
					t.Errorf("ids[%d]: got %q, want %q", i, ids[i], tc.wantIDs[i])
				}
			}
		})
	}
}
