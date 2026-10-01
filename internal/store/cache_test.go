package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aleck/agent-session-butler/internal/agent"
)

// persistentStore is newTestStore with its cache on disk, as New sets it up.
func persistentStore(path string, agents ...agent.Agent) *Store {
	return &Store{agents: agents, cache: loadCache(path), cachePath: path}
}

func enrichAll(s *Store) []agent.Session {
	var out []agent.Session
	for _, g := range s.Scan() {
		out = append(out, s.EnrichGroup(g).Sessions...)
	}
	return out
}

// The point of persisting: a second process must not re-parse an unchanged session.
func TestCacheSurvivesAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrich.json")
	f := &fakeAgent{name: "A", sessions: []agent.Session{sess("s1", "A", "/p", 100)}}
	enrichAll(persistentStore(path, f))
	if f.enriched != 1 {
		t.Fatalf("first store: %d Enrich calls, want 1", f.enriched)
	}

	// A fresh store stands in for the next CLI process.
	next := persistentStore(path, f)
	groups := next.Scan()
	if got := groups[0].Sessions[0]; got.MessageCount == nil || *got.MessageCount != 42 || got.Title != "enriched-s1" {
		t.Errorf("scan did not carry the cached enrichment: count=%v title=%q", got.MessageCount, got.Title)
	}
	next.EnrichGroup(groups[0])
	if f.enriched != 1 {
		t.Errorf("second store re-enriched an unchanged session: %d calls", f.enriched)
	}
}

func TestCacheIsInvalidatedByMtimeOrSize(t *testing.T) {
	for name, change := range map[string]func(*agent.Session){
		"mtime": func(s *agent.Session) { s.ModifiedAt = at(200) },
		"size":  func(s *agent.Session) { s.FileSize += 1 },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "enrich.json")
			f := &fakeAgent{name: "A", sessions: []agent.Session{sess("s1", "A", "/p", 100)}}
			enrichAll(persistentStore(path, f))
			change(&f.sessions[0])
			enrichAll(persistentStore(path, f))
			if f.enriched != 2 {
				t.Errorf("changed session served from cache: %d Enrich calls, want 2", f.enriched)
			}
		})
	}
}

// Only enrichment is cached. The old in-memory cache returned the whole session, so a lock taken
// or released without the transcript changing was reported as it had been at enrich time.
func TestCacheNeverServesScanFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrich.json")
	f := &fakeAgent{name: "A", sessions: []agent.Session{sess("s1", "A", "/p", 100)}}
	f.sessions[0].Locked = true
	enrichAll(persistentStore(path, f))

	f.sessions[0].Locked = false
	got := enrichAll(persistentStore(path, f))[0]
	if got.Locked {
		t.Error("Locked came from the cache rather than the current scan")
	}
}

// Codex's title comes from its index at scan time; a rename there must show even though the
// rollout, and so the cache entry, is unchanged.
func TestCacheKeepsAScannedTitleFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrich.json")
	f := &countOnlyAgent{fakeAgent{name: "A", sessions: []agent.Session{sess("s1", "A", "/p", 100)}}}
	enrichAll(persistentStore(path, f))

	f.sessions[0].Title = "renamed in the agent"
	got := enrichAll(persistentStore(path, f))[0]
	if got.Title != "renamed in the agent" {
		t.Errorf("title = %q, want the freshly scanned one", got.Title)
	}
}

// countOnlyAgent enriches the count but leaves the title, as Codex and Kiro v2 do.
type countOnlyAgent struct{ fakeAgent }

func (c *countOnlyAgent) Enrich(s agent.Session) agent.Session {
	c.enriched++
	n := 7
	s.MessageCount = &n
	return s
}

func TestUnusableCacheFileIsIgnored(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":       "{not json",
		"other version": `{"version":"0.0.0-other","entries":{"x":{"count":1}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "enrich.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := loadCache(path); len(got) != 0 {
				t.Errorf("loaded %d entries from an unusable file", len(got))
			}
		})
	}
}

// A rename changes what Enrich would read, so its entry has to go from disk too, not just memory.
func TestRenameDropsThePersistedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrich.json")
	f := &fakeAgent{name: "A", sessions: []agent.Session{sess("s1", "A", "/p", 100)}}
	s := persistentStore(path, f)
	enrichAll(s)
	key := f.sessions[0].CacheKey
	if _, ok := loadCache(path)[key]; !ok {
		t.Fatalf("precondition: no entry for %q on disk", key)
	}
	if _, err := s.RenameByID("s1", "", "", "new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCache(path)[key]; ok {
		t.Error("the renamed session's entry is still on disk")
	}
}

func TestCacheFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "enrich.json")
	enrichAll(persistentStore(path, &fakeAgent{name: "A", sessions: []agent.Session{sess("s1", "A", "/p", 100)}}))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("cache file mode %v is readable by others; titles are first prompts", perm)
	}
}
