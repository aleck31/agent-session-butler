package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aleck/agent-session-butler/internal/agent"
)

// fakeAgent is a scriptable in-memory Agent, so store behaviour (cache,
// grouping, delegation) can be tested without touching any real agent's files.
type fakeAgent struct {
	name      string
	sessions  []agent.Session
	enriched  int // how many times Enrich was called
	deleted   []string
	relocated []string
	renamed   []string
	deleteErr error
	relocErr  error
	renameErr error
}

func (f *fakeAgent) Name() string    { return f.name }
func (f *fakeAgent) Installed() bool { return true }
func (f *fakeAgent) Scan() []agent.Session {
	out := make([]agent.Session, len(f.sessions))
	copy(out, f.sessions)
	return out
}

func (f *fakeAgent) Enrich(s agent.Session) agent.Session {
	f.enriched++
	n := 42
	s.MessageCount = &n
	s.Title = "enriched-" + s.ID
	return s
}

func (f *fakeAgent) Delete(s agent.Session) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, s.ID)
	return nil
}

func (f *fakeAgent) Rename(s agent.Session, title string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	f.renamed = append(f.renamed, s.ID+"="+title)
	return nil
}

func (f *fakeAgent) Relocate(s agent.Session, newCwd string, asCopy bool) (string, error) {
	if f.relocErr != nil {
		return "", f.relocErr
	}
	f.relocated = append(f.relocated, s.ID+"→"+newCwd)
	if asCopy {
		return s.ID + "-copy", nil
	}
	return s.ID, nil
}

// uninstalledAgent must never be scanned.
type uninstalledAgent struct{ fakeAgent }

func (*uninstalledAgent) Installed() bool { return false }

func newTestStore(agents ...agent.Agent) *Store {
	return &Store{agents: agents, cache: map[string]cacheEntry{}}
}

func at(sec int64) time.Time { return time.Unix(sec, 0) }

func sess(id, agentName, cwd string, modified int64, opts ...func(*agent.Session)) agent.Session {
	s := agent.Session{
		ID: id, Agent: agentName, Cwd: cwd, Title: id,
		FileSize: 100, ModifiedAt: at(modified), CacheKey: agentName + ":" + id,
	}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func withProfile(p string) func(*agent.Session) {
	return func(s *agent.Session) {
		if s.Extra == nil {
			s.Extra = map[string]string{}
		}
		s.Extra["profile"] = p
	}
}

func locked() func(*agent.Session) { return func(s *agent.Session) { s.Locked = true } }

func withSize(n int64) func(*agent.Session) { return func(s *agent.Session) { s.FileSize = n } }

// ADR-0001 D5: grouping is keyed on (profile, cwd), so the same directory under
// two Hermes profiles forms two distinct groups — while profile-less agents
// degrade to plain cwd grouping.
func TestGroupKeysOnProfileAndCwd(t *testing.T) {
	got := group([]agent.Session{
		sess("h1", "Hermes", "/shared", 10, withProfile("x-tec")),
		sess("h2", "Hermes", "/shared", 20, withProfile("x-main")),
		sess("k1", "Kiro", "/shared", 30),
		sess("k2", "Kiro", "/shared", 40),
	})

	if len(got) != 3 {
		t.Fatalf("groups: got %d, want 3 (two Hermes profiles + one profile-less)", len(got))
	}
	counts := map[string]int{}
	for _, g := range got {
		counts[g.Profile] = len(g.Sessions)
	}
	if counts["x-tec"] != 1 || counts["x-main"] != 1 || counts[""] != 2 {
		t.Errorf("group sizes by profile: got %v", counts)
	}
}

// Sessions are newest-first inside a group, and groups are ordered by their most
// recent activity.
func TestGroupOrdersNewestFirst(t *testing.T) {
	got := group([]agent.Session{
		sess("old", "Kiro", "/a", 100),
		sess("new", "Kiro", "/a", 300),
		sess("mid", "Kiro", "/a", 200),
		sess("other", "Kiro", "/b", 250),
	})

	if len(got) != 2 {
		t.Fatalf("groups: got %d, want 2", len(got))
	}
	// /a's latest is 300, /b's is 250 → /a first.
	if got[0].Cwd != "/a" || got[1].Cwd != "/b" {
		t.Errorf("group order: got %s,%s — want /a,/b", got[0].Cwd, got[1].Cwd)
	}
	want := []string{"new", "mid", "old"}
	for i, id := range want {
		if got[0].Sessions[i].ID != id {
			t.Errorf("session %d: got %q, want %q", i, got[0].Sessions[i].ID, id)
		}
	}
}

func TestGroupOfNothingIsEmpty(t *testing.T) {
	if got := group(nil); len(got) != 0 {
		t.Errorf("got %d groups, want 0", len(got))
	}
}

func TestGroupTotalSizeAndLatestModified(t *testing.T) {
	g := Group{Sessions: []agent.Session{
		sess("a", "Kiro", "/x", 100, withSize(10)),
		sess("b", "Kiro", "/x", 300, withSize(25)),
	}}
	if got := g.TotalSize(); got != 35 {
		t.Errorf("totalSize: got %d, want 35", got)
	}
	if got := g.LatestModified(); !got.Equal(at(300)) {
		t.Errorf("latestModified: got %v, want %v", got, at(300))
	}
	if got := (Group{}).LatestModified(); !got.IsZero() {
		t.Errorf("empty group latestModified: got %v, want the zero time", got)
	}
}

func TestGroupDisplayName(t *testing.T) {
	for name, tc := range map[string]struct {
		g    Group
		want string
	}{
		"plain":           {Group{Cwd: "/home/u/proj"}, "proj"},
		"with profile":    {Group{Cwd: "/home/u/proj", Profile: "x-tec"}, "proj <x-tec>"},
		"trailing slash":  {Group{Cwd: "/home/u/proj/"}, "proj"},
		"windows sep":     {Group{Cwd: `C:\Users\u\proj`}, "proj"},
		"unknown":         {Group{Cwd: "(unknown)"}, "(unknown)"},
		"unknown+profile": {Group{Cwd: "(unknown)", Profile: "default"}, "(unknown) <default>"},
		"root falls back": {Group{Cwd: "/"}, "/"},
		"no separator":    {Group{Cwd: "relative"}, "relative"},
	} {
		if got := tc.g.DisplayName(); got != tc.want {
			t.Errorf("%s: DisplayName = %q, want %q", name, got, tc.want)
		}
	}
}

// ADR-0001 D8: orphan is one uniform rule — does the group's cwd still exist?
func TestCwdExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		cwd  string
		want bool
	}{
		"existing dir": {dir, true},
		"gone dir":     {filepath.Join(dir, "nope"), false},
		"a file":       {file, false},
		"unknown":      {"(unknown)", false},
		"empty":        {"", false},
	} {
		if got := (Group{Cwd: tc.cwd}).CwdExists(); got != tc.want {
			t.Errorf("%s: CwdExists(%q) = %v, want %v", name, tc.cwd, got, tc.want)
		}
	}
}

func TestInstalledAgentsSkipsUninstalled(t *testing.T) {
	s := newTestStore(&fakeAgent{name: "Kiro"}, &uninstalledAgent{fakeAgent{name: "Hermes"}})
	got := s.InstalledAgents()
	if len(got) != 1 || got[0] != "Kiro" {
		t.Errorf("got %v, want [Kiro]", got)
	}
}

func TestScanSkipsUninstalledAgents(t *testing.T) {
	off := &uninstalledAgent{fakeAgent{name: "Hermes", sessions: []agent.Session{sess("h", "Hermes", "/x", 1)}}}
	s := newTestStore(&fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k", "Kiro", "/x", 1)}}, off)

	groups := s.Scan()
	if len(groups) != 1 || len(groups[0].Sessions) != 1 || groups[0].Sessions[0].ID != "k" {
		t.Errorf("scan included an uninstalled agent: %+v", groups)
	}
}

// An unchanged session (same mtime) keeps its cached enrichment across scans —
// the whole point of the mtime cache.
func TestEnrichmentIsCachedAcrossScansWhileMtimeHolds(t *testing.T) {
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1000)}}
	s := newTestStore(f)

	g := s.EnrichGroup(s.Scan()[0])
	if f.enriched != 1 {
		t.Fatalf("first enrich: agent called %d times, want 1", f.enriched)
	}
	if g.Sessions[0].MessageCount == nil || *g.Sessions[0].MessageCount != 42 {
		t.Fatalf("messageCount: got %v, want 42", g.Sessions[0].MessageCount)
	}

	// A fresh scan must serve the cached enrichment, not re-read the file.
	rescanned := s.Scan()[0]
	if rescanned.Sessions[0].MessageCount == nil {
		t.Error("rescan lost the cached message count")
	}
	s.EnrichGroup(rescanned)
	if f.enriched != 1 {
		t.Errorf("agent Enrich called %d times, want 1 (cache should have served it)", f.enriched)
	}
}

// A changed mtime invalidates the cache — a session that grew must be recounted.
func TestChangedMtimeInvalidatesTheCache(t *testing.T) {
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1000)}}
	s := newTestStore(f)
	s.EnrichGroup(s.Scan()[0])

	f.sessions[0].ModifiedAt = at(2000) // the file changed on disk
	g := s.Scan()[0]
	if g.Sessions[0].MessageCount != nil {
		t.Error("stale cache entry survived an mtime change")
	}
	s.EnrichGroup(g)
	if f.enriched != 2 {
		t.Errorf("agent Enrich called %d times, want 2 (re-enriched after change)", f.enriched)
	}
}

// Scan prunes cache entries for sessions that no longer exist, so the cache
// can't grow without bound as sessions are deleted outside this process.
func TestScanPrunesCacheEntriesForGoneSessions(t *testing.T) {
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{
		sess("k1", "Kiro", "/x", 1000),
		sess("k2", "Kiro", "/x", 1000),
	}}
	s := newTestStore(f)
	s.EnrichGroup(s.Scan()[0])
	if len(s.cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(s.cache))
	}

	f.sessions = f.sessions[:1] // k2 disappeared
	s.Scan()
	if len(s.cache) != 1 {
		t.Errorf("cache size after prune: got %d, want 1", len(s.cache))
	}
	if _, ok := s.cache["Kiro:k2"]; ok {
		t.Error("cache still holds the vanished session")
	}
}

// EnrichGroup must not redo work for sessions that already carry a count.
func TestEnrichGroupSkipsAlreadyCountedSessions(t *testing.T) {
	n := 7
	pre := sess("k1", "Kiro", "/x", 1000)
	pre.MessageCount = &n
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{pre}}
	s := newTestStore(f)

	g := s.EnrichGroup(s.Scan()[0])
	if f.enriched != 0 {
		t.Errorf("agent Enrich called %d times, want 0", f.enriched)
	}
	if *g.Sessions[0].MessageCount != 7 {
		t.Errorf("messageCount: got %d, want the pre-existing 7", *g.Sessions[0].MessageCount)
	}
}

// A session from an agent that isn't in the registry is left alone rather than
// crashing the enrich pass.
func TestEnrichGroupTolreatesUnknownAgent(t *testing.T) {
	s := newTestStore(&fakeAgent{name: "Kiro"})
	g := Group{Cwd: "/x", Sessions: []agent.Session{sess("g1", "Ghost", "/x", 1)}}
	got := s.EnrichGroup(g)
	if got.Sessions[0].MessageCount != nil {
		t.Error("expected the unknown agent's session to be left unenriched")
	}
}

// A live agent owns its locked sessions — deletion must be refused before any
// agent is asked to act.
func TestDeleteRefusesLockedSessions(t *testing.T) {
	f := &fakeAgent{name: "Kiro"}
	s := newTestStore(f)

	err := s.Delete(sess("k1", "Kiro", "/x", 1, locked()))
	if err == nil {
		t.Fatal("expected an error deleting a locked session")
	}
	if len(f.deleted) != 0 {
		t.Errorf("the agent was asked to delete a locked session: %v", f.deleted)
	}
}

// ADR-0001 D7: deletion is delegated to the owning agent, and the session's
// cache entry goes with it.
func TestDeleteDelegatesAndDropsTheCacheEntry(t *testing.T) {
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1000)}}
	s := newTestStore(f)
	s.EnrichGroup(s.Scan()[0])

	if err := s.Delete(f.sessions[0]); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "k1" {
		t.Errorf("agent Delete calls: got %v, want [k1]", f.deleted)
	}
	if _, ok := s.cache["Kiro:k1"]; ok {
		t.Error("cache entry survived the delete")
	}
}

// An agent's delete failure surfaces, and must not silently drop the cache entry
// as though the delete had worked.
func TestDeletePropagatesAgentError(t *testing.T) {
	boom := errors.New("hermes delete failed")
	f := &fakeAgent{name: "Hermes", sessions: []agent.Session{sess("h1", "Hermes", "/x", 1000)}, deleteErr: boom}
	s := newTestStore(f)
	s.EnrichGroup(s.Scan()[0])

	if err := s.Delete(f.sessions[0]); !errors.Is(err, boom) {
		t.Errorf("got %v, want the agent's error", err)
	}
	if _, ok := s.cache["Hermes:h1"]; !ok {
		t.Error("a failed delete dropped the cache entry")
	}
}

func TestDeleteUnknownAgentErrors(t *testing.T) {
	s := newTestStore(&fakeAgent{name: "Kiro"})
	if err := s.Delete(sess("g", "Ghost", "/x", 1)); err == nil {
		t.Error("expected an error for a session from an unregistered agent")
	}
}

func TestDeleteByID(t *testing.T) {
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1)}}
	s := newTestStore(f)

	if _, err := s.DeleteByID("k1", "", "", false); err != nil {
		t.Fatalf("delete k1: %v", err)
	}
	if len(f.deleted) != 1 {
		t.Errorf("agent Delete calls: got %v", f.deleted)
	}
	if _, err := s.DeleteByID("nope", "", "", false); err == nil {
		t.Error("expected an error for an unknown id")
	}
}

func TestRelocateByID(t *testing.T) {
	t.Run("move keeps the id", func(t *testing.T) {
		dest := t.TempDir()
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1000)}}
		s := newTestStore(f)
		s.EnrichGroup(s.Scan()[0])

		got, resolved, err := s.RelocateByID("k1", "", "", dest, false)
		if err != nil {
			t.Fatalf("relocate: %v", err)
		}
		if got != "k1" {
			t.Errorf("id: got %q, want k1", got)
		}
		if resolved != dest {
			t.Errorf("resolved cwd: got %q, want %q", resolved, dest)
		}
		if len(f.relocated) != 1 || f.relocated[0] != "k1→"+dest {
			t.Errorf("agent Relocate calls: got %v", f.relocated)
		}
		// The old cache entry is stale once the session has moved.
		if _, ok := s.cache["Kiro:k1"]; ok {
			t.Error("stale cache entry survived the relocate")
		}
	})

	t.Run("copy gets a new id", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1)}}
		s := newTestStore(f)
		got, _, err := s.RelocateByID("k1", "", "", t.TempDir(), true)
		if err != nil {
			t.Fatalf("relocate: %v", err)
		}
		if got != "k1-copy" {
			t.Errorf("id: got %q, want k1-copy", got)
		}
	})

	t.Run("empty target cwd is rejected before any scan", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1)}}
		s := newTestStore(f)
		for _, cwd := range []string{"", "   ", "\t"} {
			if _, _, err := s.RelocateByID("k1", "", "", cwd, false); err == nil {
				t.Errorf("cwd %q: expected an error", cwd)
			}
		}
		if len(f.relocated) != 0 {
			t.Errorf("the agent was called with an empty cwd: %v", f.relocated)
		}
	})

	t.Run("locked sessions are refused", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1, locked())}}
		s := newTestStore(f)
		if _, _, err := s.RelocateByID("k1", "", "", t.TempDir(), false); err == nil {
			t.Error("expected an error relocating a locked session")
		}
		if len(f.relocated) != 0 {
			t.Errorf("the agent was asked to relocate a locked session: %v", f.relocated)
		}
	})

	t.Run("unsupported agent error surfaces", func(t *testing.T) {
		f := &fakeAgent{
			name:     "Hermes",
			sessions: []agent.Session{sess("h1", "Hermes", "/old", 1)},
			relocErr: agent.ErrRelocateUnsupported,
		}
		s := newTestStore(f)
		if _, _, err := s.RelocateByID("h1", "", "", t.TempDir(), false); !errors.Is(err, agent.ErrRelocateUnsupported) {
			t.Errorf("got %v, want ErrRelocateUnsupported", err)
		}
	})

	t.Run("unknown id errors", func(t *testing.T) {
		s := newTestStore(&fakeAgent{name: "Kiro"})
		if _, _, err := s.RelocateByID("nope", "", "", t.TempDir(), false); err == nil {
			t.Error("expected an error for an unknown id")
		}
	})
}

// The default registry must carry every supported agent, since discovery is what
// makes the tool useful out of the box.
func TestNewRegistersEveryAgent(t *testing.T) {
	s := New()
	want := map[string]bool{"Kiro": true, "Claude Code": true, "Codex": true, "Hermes": true}
	if len(s.agents) != len(want) {
		t.Fatalf("registry size: got %d, want %d", len(s.agents), len(want))
	}
	for _, a := range s.agents {
		if !want[a.Name()] {
			t.Errorf("unexpected agent %q in the default registry", a.Name())
		}
		delete(want, a.Name())
	}
	if len(want) != 0 {
		t.Errorf("missing from the default registry: %v", want)
	}
}

// A session's cwd is what lets an agent resume it and what this tool groups on,
// so a target that can never be reached must be refused rather than silently
// producing one more orphan. The realistic failure here is a typo.
func TestRelocateRejectsUnusableTargets(t *testing.T) {
	existing := t.TempDir()
	aFile := filepath.Join(existing, "notadir")
	if err := os.WriteFile(aFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]string{
		"relative path":     "relative/path",
		"bare name":         "somedir",
		"dot":               ".",
		"missing directory": filepath.Join(existing, "does-not-exist"),
		"a file":            aFile,
		"empty":             "",
		"whitespace":        "   ",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1)}}
			s := newTestStore(f)
			_, _, err := s.RelocateByID("k1", "", "", target, false)
			// "." resolves to the test's working directory, which does exist, so it
			// is accepted — the point of the case is that it is not stored verbatim.
			if name == "dot" {
				if err != nil {
					t.Fatalf("relocate: %v", err)
				}
				if len(f.relocated) != 1 || strings.HasSuffix(f.relocated[0], "→.") {
					t.Errorf("%q was not expanded before being stored: %v", target, f.relocated)
				}
				return
			}
			if err == nil {
				t.Errorf("target %q was accepted; want a rejection", target)
			}
			if len(f.relocated) != 0 {
				t.Errorf("the agent was called with an unusable target: %v", f.relocated)
			}
		})
	}
}

// A ~ must be expanded, not stored literally: the web UI and any programmatic
// caller pass it through unexpanded, and `list --path` already handles it.
func TestRelocateExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1)}}
	s := newTestStore(f)
	_, resolved, err := s.RelocateByID("k1", "", "", "~/repos", false)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	want := filepath.Join(home, "repos")
	if resolved != want {
		t.Errorf("resolved cwd: got %q, want %q", resolved, want)
	}
	if len(f.relocated) != 1 || f.relocated[0] != "k1→"+want {
		t.Errorf("the agent got an unexpanded target: %v", f.relocated)
	}
}

// A trailing slash and redundant segments are cleaned, so the stored cwd matches
// what a `list --path` query resolves to.
func TestRelocateNormalisesTheTarget(t *testing.T) {
	dest := t.TempDir()
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/old", 1)}}
	s := newTestStore(f)

	_, resolved, err := s.RelocateByID("k1", "", "", dest+string(filepath.Separator)+"."+string(filepath.Separator), false)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if resolved != dest {
		t.Errorf("resolved cwd: got %q, want %q", resolved, dest)
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	for name, tc := range map[string]struct{ in, want string }{
		"tilde alone":    {"~", home},
		"tilde slash":    {"~/", home},
		"under home":     {"~/proj", filepath.Join(home, "proj")},
		"absolute":       {"/a/b", filepath.Join("/a", "b")},
		"trailing slash": {"/a/b/", filepath.Join("/a", "b")},
		"dot segments":   {"/a/./b/../b", filepath.Join("/a", "b")},
		"literal tilde":  {"~notahome", ""}, // relative → becomes cwd-relative
	} {
		got, err := ExpandPath(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s: ExpandPath(%q) = %q, want an absolute path", name, tc.in, got)
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("%s: ExpandPath(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// Renaming writes into the owning agent's own metadata, so it is delegated and
// the cached title has to go with it.
func TestRenameByID(t *testing.T) {
	t.Run("delegates and returns the trimmed title", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1000)}}
		s := newTestStore(f)
		s.EnrichGroup(s.Scan()[0])

		got, err := s.RenameByID("k1", "", "", "  a better name  ")
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		if got != "a better name" {
			t.Errorf("title: got %q, want the trimmed form", got)
		}
		if len(f.renamed) != 1 || f.renamed[0] != "k1=a better name" {
			t.Errorf("agent Rename calls: got %v", f.renamed)
		}
		if _, ok := s.cache["Kiro:k1"]; ok {
			t.Error("the stale cached title survived the rename")
		}
	})

	t.Run("blank titles are refused before the agent is called", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1)}}
		s := newTestStore(f)
		for _, title := range []string{"", "   ", "\t\n"} {
			if _, err := s.RenameByID("k1", "", "", title); err == nil {
				t.Errorf("title %q: expected an error", title)
			}
		}
		if len(f.renamed) != 0 {
			t.Errorf("the agent was called with a blank title: %v", f.renamed)
		}
	})

	// No length cap: agents store titles far longer than any sensible limit, so
	// capping would make it impossible to put back a title that was already there.
	t.Run("a very long title is accepted", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1)}}
		s := newTestStore(f)
		long := strings.Repeat("题", 5000)
		got, err := s.RenameByID("k1", "", "", long)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		if got != long {
			t.Errorf("title was altered: %d runes in, %d out", len([]rune(long)), len([]rune(got)))
		}
	})

	t.Run("locked sessions are refused", func(t *testing.T) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{sess("k1", "Kiro", "/x", 1, locked())}}
		s := newTestStore(f)
		if _, err := s.RenameByID("k1", "", "", "new"); err == nil {
			t.Error("expected an error renaming a locked session")
		}
		if len(f.renamed) != 0 {
			t.Errorf("the agent was asked to rename a locked session: %v", f.renamed)
		}
	})

	t.Run("agent errors surface and keep the cache", func(t *testing.T) {
		boom := errors.New("hermes rename failed")
		f := &fakeAgent{name: "Hermes", sessions: []agent.Session{sess("h1", "Hermes", "/x", 1000)}, renameErr: boom}
		s := newTestStore(f)
		s.EnrichGroup(s.Scan()[0])
		if _, err := s.RenameByID("h1", "", "", "new"); !errors.Is(err, boom) {
			t.Errorf("got %v, want the agent's error", err)
		}
		if _, ok := s.cache["Hermes:h1"]; !ok {
			t.Error("a failed rename dropped the cache entry")
		}
	})

	t.Run("unsupported agents surface their error", func(t *testing.T) {
		f := &fakeAgent{name: "Ghost", sessions: []agent.Session{sess("g1", "Ghost", "/x", 1)},
			renameErr: agent.ErrRenameUnsupported}
		s := newTestStore(f)
		if _, err := s.RenameByID("g1", "", "", "new"); !errors.Is(err, agent.ErrRenameUnsupported) {
			t.Errorf("got %v, want ErrRenameUnsupported", err)
		}
	})

	t.Run("unknown id errors", func(t *testing.T) {
		s := newTestStore(&fakeAgent{name: "Kiro"})
		if _, err := s.RenameByID("nope", "", "", "new"); err == nil {
			t.Error("expected an error for an unknown id")
		}
	})
}

// An id that matches sessions in two stores must never be acted on by guesswork:
// Kiro copies a v1 session into v2 under the same id, so picking whichever the
// scan reached first would delete the wrong one half the time.
func TestAmbiguousIDIsRefusedRatherThanGuessed(t *testing.T) {
	v1 := sess("shared", "Kiro", "/proj", 1000)
	v1.Store = "v1"
	v1.CacheKey = "db#v1#shared"
	v2 := sess("shared", "Kiro", "/proj", 2000)
	v2.Store = "v2"
	v2.CacheKey = "file#shared"
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{v1, v2}}
	s := newTestStore(f)
	dest := t.TempDir()

	t.Run("delete refuses", func(t *testing.T) {
		_, err := s.DeleteByID("shared", "", "", false)
		if !errors.Is(err, ErrAmbiguousID) {
			t.Errorf("got %v, want ErrAmbiguousID", err)
		}
		if len(f.deleted) != 0 {
			t.Errorf("a session was deleted despite the ambiguity: %v", f.deleted)
		}
		// The message has to name the stores, or the caller cannot act on it.
		if err != nil && !strings.Contains(err.Error(), "v1") {
			t.Errorf("error %q should name the stores", err)
		}
	})

	t.Run("delete with a store acts on that one only", func(t *testing.T) {
		f.deleted = nil
		if _, err := s.DeleteByID("shared", "v1", "", false); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if len(f.deleted) != 1 {
			t.Fatalf("agent Delete calls: %v", f.deleted)
		}
	})

	t.Run("rename refuses", func(t *testing.T) {
		f.renamed = nil
		if _, err := s.RenameByID("shared", "", "", "new"); !errors.Is(err, ErrAmbiguousID) {
			t.Errorf("got %v, want ErrAmbiguousID", err)
		}
		if len(f.renamed) != 0 {
			t.Errorf("a session was renamed despite the ambiguity: %v", f.renamed)
		}
	})

	t.Run("relocate refuses", func(t *testing.T) {
		f.relocated = nil
		if _, _, err := s.RelocateByID("shared", "", "", dest, false); !errors.Is(err, ErrAmbiguousID) {
			t.Errorf("got %v, want ErrAmbiguousID", err)
		}
		if len(f.relocated) != 0 {
			t.Errorf("a session was relocated despite the ambiguity: %v", f.relocated)
		}
	})

	t.Run("an unknown store is not silently ignored", func(t *testing.T) {
		_, err := s.DeleteByID("shared", "v9", "", false)
		if err == nil {
			t.Error("expected an error for a store that has no such session")
		}
		if errors.Is(err, ErrAmbiguousID) {
			t.Error("a narrowed lookup that matches nothing is not an ambiguity")
		}
	})
}

// A unique id needs no store, so the common case stays simple.
func TestUnambiguousIDNeedsNoStore(t *testing.T) {
	only := sess("solo", "Kiro", "/proj", 1000)
	only.Store = "v2"
	f := &fakeAgent{name: "Kiro", sessions: []agent.Session{only}}
	s := newTestStore(f)
	if _, err := s.DeleteByID("solo", "", "", false); err != nil {
		t.Errorf("delete without a store: %v", err)
	}
}

// Kiro's v1 store keys a conversation by (cwd, id), so one id can name several
// different conversations. Its CLI deletes by id alone, which makes a per-cwd
// delete impossible — the tool has to say so rather than appear to do one.
func TestSameStoreDuplicateIDNeedsCwdOrConfirmation(t *testing.T) {
	a := sess("dupe", "Kiro", "/proj/a", 1000)
	a.Store, a.CacheKey = "v1", "db#v1#/proj/a#dupe"
	a.Extra = map[string]string{"deleteScope": "id"}
	b := sess("dupe", "Kiro", "/proj/b", 2000)
	b.Store, b.CacheKey = "v1", "db#v1#/proj/b#dupe"
	b.Extra = map[string]string{"deleteScope": "id"}

	newStore := func() (*Store, *fakeAgent) {
		f := &fakeAgent{name: "Kiro", sessions: []agent.Session{a, b}}
		return newTestStore(f), f
	}

	t.Run("a store alone does not disambiguate, and the message says so", func(t *testing.T) {
		s, _ := newStore()
		_, _, err := s.findSession("dupe", "v1", "")
		if !errors.Is(err, ErrAmbiguousID) {
			t.Fatalf("got %v, want ErrAmbiguousID", err)
		}
		// Pointing at --store here would send the caller in a circle.
		if strings.Contains(err.Error(), "--store") || !strings.Contains(err.Error(), "--path") {
			t.Errorf("error %q should point at --path, not --store", err)
		}
		for _, dir := range []string{"/proj/a", "/proj/b"} {
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("error %q should name %s", err, dir)
			}
		}
	})

	t.Run("a cwd picks one for reads", func(t *testing.T) {
		s, _ := newStore()
		_, got, err := s.findSession("dupe", "v1", "/proj/b")
		if err != nil {
			t.Fatalf("findSession: %v", err)
		}
		if got.Cwd != "/proj/b" {
			t.Errorf("cwd = %q, want /proj/b", got.Cwd)
		}
	})

	t.Run("delete refuses without confirmation", func(t *testing.T) {
		s, f := newStore()
		_, err := s.DeleteByID("dupe", "v1", "", false)
		if !errors.Is(err, ErrAmbiguousID) {
			t.Fatalf("got %v, want ErrAmbiguousID", err)
		}
		if !strings.Contains(err.Error(), "--all-with-id") {
			t.Errorf("error %q should name the flag that proceeds", err)
		}
		if len(f.deleted) != 0 {
			t.Errorf("deleted despite refusing: %v", f.deleted)
		}
	})

	// The trap this guards: a cwd makes the delete look scoped, but the agent still
	// removes every session with the id, so the other one would vanish unannounced.
	t.Run("a cwd does not make the delete look precise", func(t *testing.T) {
		s, f := newStore()
		_, err := s.DeleteByID("dupe", "v1", "/proj/a", false)
		if !errors.Is(err, ErrAmbiguousID) {
			t.Fatalf("a cwd-narrowed delete must still refuse, got %v", err)
		}
		if len(f.deleted) != 0 {
			t.Errorf("deleted despite refusing: %v", f.deleted)
		}
	})

	t.Run("confirmation deletes once and reports every session that went", func(t *testing.T) {
		s, f := newStore()
		// Populate the cache so the purge can be checked.
		s.cache[a.CacheKey] = cacheEntry{MTime: a.ModifiedAt, Size: a.FileSize, Count: 1}
		s.cache[b.CacheKey] = cacheEntry{MTime: b.ModifiedAt, Size: b.FileSize, Count: 1}

		removed, err := s.DeleteByID("dupe", "v1", "", true)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if len(removed) != 2 {
			t.Errorf("reported %d removed, want 2 — the caller's list is staler than it knows", len(removed))
		}
		// One CLI call takes both; calling twice would fail on the second.
		if len(f.deleted) != 1 {
			t.Errorf("agent Delete calls = %d, want 1", len(f.deleted))
		}
		for _, k := range []string{a.CacheKey, b.CacheKey} {
			if _, stale := s.cache[k]; stale {
				t.Errorf("cache entry %q survived the delete", k)
			}
		}
	})
}

// resumer echoes whether the store judged its id shared, so the judgement itself can be checked.
type resumer struct{ fakeAgent }

func (r *resumer) ResumeArgv(s agent.Session, shared bool) []string {
	if shared {
		return []string{"resume", s.ID, "shared"}
	}
	return []string{"resume", s.ID}
}

// Shared means the id is in another store of the same agent — wherever that copy's cwd is, so a
// --path listing still disambiguates a v1 row whose v2 copy sits under another spelling of the dir.
func TestResumeSeesCopiesOutsideTheDirectory(t *testing.T) {
	v1 := sess("dup", "K", "/home/u/workplace", 100)
	v1.Store = "v1"
	v2 := sess("dup", "K", "/home/u/quickspace", 100)
	v2.Store, v2.CacheKey = "v2", "K:dup:v2"
	solo := sess("solo", "K", "/home/u/workplace", 100)
	solo.Store = "v1"
	other := sess("dup", "Other", "/home/u/workplace", 100) // same id, different agent
	s := newTestStore(&resumer{fakeAgent{name: "K", sessions: []agent.Session{v1, v2, solo}}},
		&fakeAgent{name: "Other", sessions: []agent.Session{other}})

	got := map[string][]string{}
	for _, g := range s.Scan() {
		for _, x := range g.Sessions {
			got[x.Agent+"/"+x.ID+"/"+x.Store] = x.Resume
		}
	}
	for key, want := range map[string][]string{
		"K/dup/v1":   {"resume", "dup", "shared"},
		"K/dup/v2":   {"resume", "dup", "shared"},
		"K/solo/v1":  {"resume", "solo"},
		"Other/dup/": nil, // not a Resumer, and the K rows do not make it shared
	} {
		if strings.Join(got[key], " ") != strings.Join(want, " ") {
			t.Errorf("%s: resume = %v, want %v", key, got[key], want)
		}
	}
}
