package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aleck/agent-session-butler/internal/agent"
)

// The enrichment cache persists across processes: the CLI is a fresh process per `list`, so an
// in-memory cache never helped it, and a consumer switching directories paid the full parse every
// time. Entries are validated by mtime and size, never by age, so nothing is ever served stale.

// cacheEntry is only what Enrich adds; everything else, Locked included, comes from the current scan.
type cacheEntry struct {
	MTime time.Time `json:"mtime"`
	Size  int64     `json:"size"`
	Count int       `json:"count"`
	// Title is nil when Enrich left the scanned title alone, so a title the scan reads fresh
	// (Codex's, from its index) is never overridden by an old copy.
	Title *string `json:"title,omitempty"`
}

func (e cacheEntry) valid(s agent.Session) bool {
	return e.MTime.Equal(s.ModifiedAt) && e.Size == s.FileSize
}

func (e cacheEntry) apply(s agent.Session) agent.Session {
	n := e.Count
	s.MessageCount = &n
	if e.Title != nil {
		s.Title = *e.Title
	}
	return s
}

// entryFor records what Enrich changed between the scanned and enriched forms of a session.
func entryFor(scanned, enriched agent.Session) (cacheEntry, bool) {
	if enriched.MessageCount == nil {
		return cacheEntry{}, false
	}
	e := cacheEntry{MTime: enriched.ModifiedAt, Size: enriched.FileSize, Count: *enriched.MessageCount}
	if enriched.Title != scanned.Title {
		t := enriched.Title
		e.Title = &t
	}
	return e, true
}

type cacheFile struct {
	// Version invalidates the whole file on upgrade, since a new release may count differently.
	Version string                `json:"version"`
	Entries map[string]cacheEntry `json:"entries"`
}

// defaultCachePath sits beside the update check's state, in the platform's user cache directory.
func defaultCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "asbutler", "enrich.json")
}

// loadCache treats any unreadable, corrupt or other-version file as empty: it is only a cache.
func loadCache(path string) map[string]cacheEntry {
	entries := map[string]cacheEntry{}
	if path == "" {
		return entries
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return entries
	}
	var f cacheFile
	if json.Unmarshal(data, &f) != nil || f.Version != agent.Version || f.Entries == nil {
		return entries
	}
	return f.Entries
}

// saveLocked writes the cache atomically, so a concurrent reader never sees a torn file; between
// concurrent writers the last one wins, which costs only a recount. Caller holds s.mu.
func (s *Store) saveLocked() {
	if s.cachePath == "" || !s.dirty {
		return
	}
	data, err := json.Marshal(cacheFile{Version: agent.Version, Entries: s.cache})
	if err != nil {
		return
	}
	dir := filepath.Dir(s.cachePath)
	// Titles are first prompts, so keep the file private on a shared host.
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "enrich-*.json")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), s.cachePath) != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	s.dirty = false
}
