// Package store aggregates every installed agent's sessions, groups them by
// working directory, caches enrichment by mtime, and handles deletion.
package store

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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
			agent.CodexAgent{},
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

// ErrAmbiguousID is returned when an id matches sessions in more than one store
// and the caller did not say which. Kiro copies a v1 session into v2 under the
// same id, so guessing would mean acting on whichever the scan happened to reach
// first — and for a delete, that is the wrong session half the time.
var ErrAmbiguousID = errors.New("id matches more than one session")

// findSession locates a session by id, narrowed by store when given. An empty
// store matches any, but only if exactly one session matches.
func (s *Store) findSession(id, store string) (Group, agent.Session, error) {
	var hits []agent.Session
	var groups []Group
	for _, g := range s.Scan() {
		for _, sess := range g.Sessions {
			if sess.ID != id {
				continue
			}
			if store != "" && sess.Store != store {
				continue
			}
			hits = append(hits, sess)
			groups = append(groups, g)
		}
	}
	switch len(hits) {
	case 0:
		if store != "" {
			return Group{}, agent.Session{}, fmt.Errorf("no session with id %q in the %s store", id, store)
		}
		return Group{}, agent.Session{}, fmt.Errorf("no session with id %q", id)
	case 1:
		return groups[0], hits[0], nil
	default:
		var stores []string
		for _, h := range hits {
			stores = append(stores, h.Store)
		}
		return Group{}, agent.Session{}, fmt.Errorf("%w: %q is in the %s stores — say which with a store",
			ErrAmbiguousID, id, strings.Join(stores, " and "))
	}
}

// DeleteByID finds a session by its id (via a fresh scan) and deletes it.
// Returns an error if no session matches or the delete fails. Used by callers
// that only have an id (e.g. the HTTP layer).
func (s *Store) DeleteByID(id, store string) error {
	_, sess, err := s.findSession(id, store)
	if err != nil {
		return err
	}
	return s.Delete(sess)
}

// RelocateByID moves (or copies, if asCopy) the session with the given id to
// newCwd, delegating to the owning agent. Returns the resulting session id and
// the normalised cwd it was actually filed under — the target is expanded and
// validated, so what gets stored is not necessarily the string passed in.
// Refuses locked sessions and targets that are not an existing directory.
func (s *Store) RelocateByID(id, store, newCwd string, asCopy bool) (newID, resolvedCwd string, err error) {
	target, err := resolveTargetCwd(newCwd)
	if err != nil {
		return "", "", err
	}
	_, sess, err := s.findSession(id, store)
	if err != nil {
		return "", "", err
	}
	if sess.Locked {
		return "", "", fmt.Errorf("session is in use by a running process")
	}
	a := s.agentNamed(sess.Agent)
	if a == nil {
		return "", "", fmt.Errorf("unknown agent %q", sess.Agent)
	}
	newID, err = a.Relocate(sess, target, asCopy)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	delete(s.cache, sess.CacheKey) // moved/copied → old cache entry is stale
	s.mu.Unlock()
	return newID, target, nil
}

// RenameByID sets a session's title in its owning agent's own metadata, so the
// agent shows the new title too. Refuses locked sessions and blank titles.
// Returns the trimmed title that was actually written.
func (s *Store) RenameByID(id, store, title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", fmt.Errorf("title must not be empty")
	}
	// Deliberately no length cap. An earlier one at 200 runes looked sensible until
	// it made the operation non-round-trippable: agents store far longer titles
	// themselves, so a cap rejects putting back a title that was already there.
	_, sess, err := s.findSession(id, store)
	if err != nil {
		return "", err
	}
	if sess.Locked {
		return "", fmt.Errorf("session is in use by a running process")
	}
	a := s.agentNamed(sess.Agent)
	if a == nil {
		return "", fmt.Errorf("unknown agent %q", sess.Agent)
	}
	if err := a.Rename(sess, t); err != nil {
		return "", err
	}
	s.mu.Lock()
	delete(s.cache, sess.CacheKey) // the cached title is now stale
	s.mu.Unlock()
	return t, nil
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

// ErrTranscriptUnsupported is returned for agents that cannot render a
// conversation. None currently, but the Reader interface is separate from Agent
// so a new backend can be listed and cleaned up before it can be read.
var ErrTranscriptUnsupported = errors.New("this agent cannot show session contents")

// Transcript returns a session's conversation. Sessions reach 97 MB, so opts
// bounds what is read; with no bound the caller has opted into the whole thing.
func (s *Store) TranscriptByID(id, store string, opts agent.TranscriptOptions) (agent.Session, []agent.Turn, error) {
	g, sess, err := s.findSession(id, store)
	if err != nil {
		return agent.Session{}, nil, err
	}
	a := s.agentNamed(sess.Agent)
	if a == nil {
		return sess, nil, fmt.Errorf("unknown agent %q", sess.Agent)
	}
	r, ok := a.(agent.Reader)
	if !ok {
		return sess, nil, ErrTranscriptUnsupported
	}
	// Enrich first: the title is a lazy field, and a viewer that heads its output
	// with a filename or a placeholder is showing the wrong thing.
	enriched := s.EnrichGroup(Group{Cwd: g.Cwd, Profile: g.Profile,
		Sessions: []agent.Session{sess}}).Sessions[0]
	turns, err := r.Transcript(enriched, opts)
	return enriched, turns, err
}
