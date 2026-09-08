package main

import (
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
