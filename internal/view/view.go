// Package view turns the store's scan results into JSON views. Two shapes share
// the same underlying Session data:
//   - Grouped: nested groups → for the web UI, which renders by directory.
//   - Flat:    one self-contained session array + summary → for the CLI/agents,
//     which filter and pick ids without wanting to walk a nesting.
package view

import (
	"time"

	"github.com/aleck/agent-session-butler/internal/store"
)

// Session is one session, self-contained: it carries its own cwd/profile/orphan
// so a flat list needs no back-reference to a group. Both byte and human sizes
// and an RFC3339 timestamp are provided so callers don't reparse.
type Session struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Cwd     string `json:"cwd"`
	Profile string `json:"profile"` // "" for agents without profiles (Kiro/Claude)
	// Store distinguishes rows that share an id because the agent keeps several
	// stores; "" when it keeps one. Not part of the grouping key: a directory's v1
	// and v2 sessions belong side by side, which is how you spot a stale duplicate.
	Store        string    `json:"store"`
	Orphan       bool      `json:"orphan"` // working directory no longer exists
	Title        string    `json:"title"`
	MessageCount *int      `json:"messageCount"`
	FileSize     int64     `json:"fileSize"`
	SizeHuman    string    `json:"sizeHuman"`
	ModifiedAt   time.Time `json:"modifiedAt"` // RFC3339
	Locked       bool      `json:"locked"`
	// Resume is the argv that reopens the session, run from cwd; absent when the agent has none.
	Resume []string `json:"resume,omitempty"`
}

// AgentUsage is one segment of the disk-usage bar: an agent's live (non-orphan)
// bytes. Orphaned bytes are reported separately in Summary.OrphanSize.
type AgentUsage struct {
	Agent     string `json:"agent"`
	LiveSize  int64  `json:"liveSize"`
	SizeHuman string `json:"sizeHuman"`
}

// Summary is the top-level rollup shared by both views.
type Summary struct {
	Version     string       `json:"version,omitempty"`
	Agents      []string     `json:"agents"`
	AgentUsage  []AgentUsage `json:"agentUsage"`
	TotalGroups int          `json:"totalGroups"`
	TotalCount  int          `json:"totalCount"`
	TotalSize   int64        `json:"totalSize"`
	OrphanCount int          `json:"orphanCount"`
	OrphanSize  int64        `json:"orphanSize"`
}

// Group is one cwd+profile group with its sessions (nested/web view).
type Group struct {
	Cwd            string    `json:"cwd"`
	Profile        string    `json:"profile"`
	DisplayName    string    `json:"displayName"`
	Orphan         bool      `json:"orphan"`
	SessionCount   int       `json:"sessionCount"`
	TotalSize      int64     `json:"totalSize"`
	TotalSizeHuman string    `json:"totalSizeHuman"`
	LatestModified time.Time `json:"latestModified"`
	Sessions       []Session `json:"sessions"`
}

// GroupedResult is Summary + nested Groups (web UI shape).
type GroupedResult struct {
	Summary
	Groups []Group `json:"groups"`
}

// FlatResult is Summary + a flat, self-contained session list (CLI/agent shape).
type FlatResult struct {
	Summary  Summary   `json:"summary"`
	Sessions []Session `json:"sessions"`
}

// sessionsOf builds self-contained session views for a group.
func sessionsOf(g store.Group) []Session {
	orphan := !g.CwdExists()
	out := make([]Session, 0, len(g.Sessions))
	for _, s := range g.Sessions {
		out = append(out, Session{
			ID:           s.ID,
			Agent:        s.Agent,
			Cwd:          g.Cwd,
			Profile:      g.Profile,
			Store:        s.Store,
			Orphan:       orphan,
			Title:        s.Title,
			MessageCount: s.MessageCount,
			FileSize:     s.FileSize,
			SizeHuman:    store.HumanSize(s.FileSize),
			ModifiedAt:   s.ModifiedAt,
			Locked:       s.Locked,
			Resume:       s.Resume,
		})
	}
	return out
}

// summaryOf computes the rollup common to both views.
func summaryOf(agents []string, groups []store.Group, version string) Summary {
	// Empty slices (not nil) so JSON emits [] not null — callers can filter/len
	// without a nil guard.
	sum := Summary{Version: version, Agents: agents, AgentUsage: []AgentUsage{}}
	liveByAgent := map[string]int64{}
	for _, g := range groups {
		orphan := !g.CwdExists()
		total := g.TotalSize()
		sum.TotalGroups++
		sum.TotalCount += len(g.Sessions)
		sum.TotalSize += total
		if orphan {
			sum.OrphanCount++
			sum.OrphanSize += total
		} else {
			for _, s := range g.Sessions {
				liveByAgent[s.Agent] += s.FileSize
			}
		}
	}
	// Live segments in installed-agent order (stable colours in the UI).
	for _, name := range agents {
		if sz := liveByAgent[name]; sz > 0 {
			sum.AgentUsage = append(sum.AgentUsage, AgentUsage{Agent: name, LiveSize: sz, SizeHuman: store.HumanSize(sz)})
		}
	}
	return sum
}

// Grouped builds the nested view (web UI).
func Grouped(agents []string, groups []store.Group, version string) GroupedResult {
	r := GroupedResult{Summary: summaryOf(agents, groups, version)}
	r.Groups = make([]Group, 0, len(groups))
	for _, g := range groups {
		r.Groups = append(r.Groups, Group{
			Cwd:            g.Cwd,
			Profile:        g.Profile,
			DisplayName:    g.DisplayName(),
			Orphan:         !g.CwdExists(),
			SessionCount:   len(g.Sessions),
			TotalSize:      g.TotalSize(),
			TotalSizeHuman: store.HumanSize(g.TotalSize()),
			LatestModified: g.LatestModified(),
			Sessions:       sessionsOf(g),
		})
	}
	return r
}

// GroupView builds one group's nested view (used by the enrich endpoint).
func GroupView(g store.Group) Group {
	return Group{
		Cwd:            g.Cwd,
		Profile:        g.Profile,
		DisplayName:    g.DisplayName(),
		Orphan:         !g.CwdExists(),
		SessionCount:   len(g.Sessions),
		TotalSize:      g.TotalSize(),
		TotalSizeHuman: store.HumanSize(g.TotalSize()),
		LatestModified: g.LatestModified(),
		Sessions:       sessionsOf(g),
	}
}

// Flat builds the flat view (CLI/agents): a summary plus a self-contained
// session array, newest first.
func Flat(agents []string, groups []store.Group, version string) FlatResult {
	r := FlatResult{Summary: summaryOf(agents, groups, version), Sessions: []Session{}}
	for _, g := range groups {
		r.Sessions = append(r.Sessions, sessionsOf(g)...)
	}
	return r
}
