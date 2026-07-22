// Package store aggregates every installed agent's sessions, groups them by
// working directory, caches enrichment by mtime, and handles deletion.
package store

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aleck/agent-session-butler/internal/agent"
)

// Group is a set of sessions sharing a working directory (and, for agents that
// have profiles like Hermes, the same profile — so the same cwd under different
// profiles forms distinct groups).
type Group struct {
	Cwd      string          `json:"cwd"`
	Profile  string          `json:"profile"` // "" for agents without profiles
	Sessions []agent.Session `json:"sessions"`
}

// DisplayName is the cwd's last path component, tagged with the profile when
// present (e.g. "agent-manager <x-tec>").
func (g Group) DisplayName() string {
	name := lastPathComponent(g.Cwd)
	if name == "" {
		name = g.Cwd
	}
	if g.Profile != "" {
		return name + " <" + g.Profile + ">"
	}
	return name
}

func (g Group) TotalSize() int64 {
	var sum int64
	for _, s := range g.Sessions {
		sum += s.FileSize
	}
	return sum
}

func (g Group) LatestModified() time.Time {
	var latest time.Time
	for _, s := range g.Sessions {
		if s.ModifiedAt.After(latest) {
			latest = s.ModifiedAt
		}
	}
	return latest
}

// CwdExists reports whether the working directory still exists on disk. When
// false this is an "orphan" group — the project is gone but its sessions
// linger, making it a prime cleanup candidate.
func (g Group) CwdExists() bool {
	if g.Cwd == "(unknown)" {
		return false
	}
	info, err := os.Stat(g.Cwd)
	return err == nil && info.IsDir()
}

type cacheEntry struct {
	mtime   time.Time
	session agent.Session
}

// Store owns the agent registry and the enrichment cache.
type Store struct {
	agents []agent.Agent

	mu    sync.RWMutex
	cache map[string]cacheEntry // keyed by CacheKey
}

// New builds a store with the default agent registry. Order is cosmetic only
// (a stable tie-breaker).
func New() *Store {
	return &Store{
		agents: []agent.Agent{
			agent.KiroAgent{},
			agent.ClaudeCodeAgent{},
			agent.HermesAgent{},
		},
		cache: map[string]cacheEntry{},
	}
}

// InstalledAgents returns the names of agents detected on this machine.
func (s *Store) InstalledAgents() []string {
	var names []string
	for _, a := range s.agents {
		if a.Installed() {
			names = append(names, a.Name())
		}
	}
	return names
}

func (s *Store) agentNamed(name string) agent.Agent {
	for _, a := range s.agents {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

// Scan does a full concurrent re-scan of all installed agents. Unchanged files
// (same mtime) keep their cached enrichment; the cache is pruned to what still
// exists on disk. Returns groups sorted by most recent activity.
func (s *Store) Scan() []Group {
	installed := make([]agent.Agent, 0, len(s.agents))
	for _, a := range s.agents {
		if a.Installed() {
			installed = append(installed, a)
		}
	}

	// Scan each agent concurrently — each reads a distinct directory tree.
	results := make([][]agent.Session, len(installed))
	var wg sync.WaitGroup
	for i, a := range installed {
		wg.Add(1)
		go func(i int, a agent.Agent) {
			defer wg.Done()
			results[i] = a.Scan()
		}(i, a)
	}
	wg.Wait()

	var scanned []agent.Session
	for _, r := range results {
		scanned = append(scanned, r...)
	}

	s.mu.Lock()
	merged := make([]agent.Session, len(scanned))
	live := make(map[string]struct{}, len(scanned))
	for i, sc := range scanned {
		merged[i] = s.mergedLocked(sc)
		live[sc.CacheKey] = struct{}{}
	}
	// Drop cache entries for files that no longer exist.
	for k := range s.cache {
		if _, ok := live[k]; !ok {
			delete(s.cache, k)
		}
	}
	s.mu.Unlock()

	return group(merged)
}

// mergedLocked reuses the cached enriched session when the file is unchanged
// (same mtime); otherwise returns the freshly-scanned session as-is. Caller
// holds s.mu.
func (s *Store) mergedLocked(scanned agent.Session) agent.Session {
	if hit, ok := s.cache[scanned.CacheKey]; ok && hit.mtime.Equal(scanned.ModifiedAt) {
		return hit.session
	}
	return scanned
}

// EnrichGroup enriches every not-yet-counted session in a group (message count
// and, for some agents, the title), caching the results by mtime. Returns the
// group with enriched sessions patched in.
func (s *Store) EnrichGroup(g Group) Group {
	for i, sess := range g.Sessions {
		if sess.MessageCount != nil {
			continue
		}
		s.mu.RLock()
		hit, ok := s.cache[sess.CacheKey]
		s.mu.RUnlock()
		if ok && hit.mtime.Equal(sess.ModifiedAt) {
			g.Sessions[i] = hit.session
			continue
		}
		a := s.agentNamed(sess.Agent)
		if a == nil {
			continue
		}
		enriched := a.Enrich(sess)
		g.Sessions[i] = enriched
		s.mu.Lock()
		s.cache[enriched.CacheKey] = cacheEntry{mtime: enriched.ModifiedAt, session: enriched}
		s.mu.Unlock()
	}
	return g
}

// Delete permanently removes a session, delegating to the owning agent (file
// agents remove files; Hermes shells out to its CLI). Refuses locked sessions
// (a live agent owns them). Returns an error message, or nil on success.
func (s *Store) Delete(sess agent.Session) error {
	if sess.Locked {
		return fmt.Errorf("session is in use by a running process")
	}
	a := s.agentNamed(sess.Agent)
	if a == nil {
		return fmt.Errorf("unknown agent %q", sess.Agent)
	}
	if err := a.Delete(sess); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cache, sess.CacheKey)
	s.mu.Unlock()
	return nil
}

// DeleteByID finds a session by its id (via a fresh scan) and deletes it.
// Returns an error if no session matches or the delete fails. Used by callers
// that only have an id (e.g. the HTTP layer).
func (s *Store) DeleteByID(id string) error {
	for _, g := range s.Scan() {
		for _, sess := range g.Sessions {
			if sess.ID == id {
				return s.Delete(sess)
			}
		}
	}
	return fmt.Errorf("no session with id %q", id)
}

// group buckets sessions by cwd; newest session first within a group, and
// groups ordered by their most recent activity.
func group(sessions []agent.Session) []Group {
	// Key by profile + cwd so the same directory under different Hermes profiles
	// forms distinct groups. Agents without profiles use an empty profile, so
	// this degrades to plain cwd grouping for Kiro/Claude Code.
	type key struct{ profile, cwd string }
	byKey := map[key][]agent.Session{}
	for _, s := range sessions {
		byKey[key{s.Extra["profile"], s.Cwd}] = append(byKey[key{s.Extra["profile"], s.Cwd}], s)
	}
	groups := make([]Group, 0, len(byKey))
	for k, items := range byKey {
		sort.Slice(items, func(i, j int) bool {
			return items[i].ModifiedAt.After(items[j].ModifiedAt)
		})
		groups = append(groups, Group{Cwd: k.cwd, Profile: k.profile, Sessions: items})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].LatestModified().After(groups[j].LatestModified())
	})
	return groups
}
