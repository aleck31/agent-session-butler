package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aleck/agent-session-butler/internal/agent"
	"github.com/aleck/agent-session-butler/internal/store"
)

func intp(n int) *int { return &n }

var fixedTime = time.Date(2026, 7, 22, 22, 29, 0, 0, time.UTC)

// liveGroup builds a group whose cwd exists on disk (so Orphan is false).
func liveGroup(t *testing.T, sessions ...agent.Session) store.Group {
	t.Helper()
	dir := t.TempDir()
	for i := range sessions {
		sessions[i].Cwd = dir
	}
	return store.Group{Cwd: dir, Sessions: sessions}
}

func session(id, agentName string, size int64, msgs int) agent.Session {
	return agent.Session{
		ID: id, Agent: agentName, Title: "t-" + id,
		MessageCount: intp(msgs), FileSize: size, ModifiedAt: fixedTime,
	}
}

// TestFlatContract pins the agent-facing JSON contract. Downstream consumers
// (see README "Downstream consumers") parse these exact keys — a rename or
// removal here is a breaking change and must bump the version and notify them.
func TestFlatContract(t *testing.T) {
	g := liveGroup(t, session("s1", "Kiro", 2048, 12))
	got := Flat([]string{"Kiro"}, []store.Group{g}, "0.6.1")

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Top level is exactly {summary, sessions} — a flat, self-contained shape.
	assertKeys(t, "top level", doc, "summary", "sessions")
	assertKeys(t, "summary", doc["summary"].(map[string]any),
		"version", "agents", "agentUsage", "totalGroups", "totalCount",
		"totalSize", "orphanCount", "orphanSize")

	sessions := doc["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions: got %d, want 1", len(sessions))
	}
	assertKeys(t, "session", sessions[0].(map[string]any),
		"id", "agent", "cwd", "profile", "orphan", "title",
		"messageCount", "fileSize", "sizeHuman", "modifiedAt", "locked")

	// Every session carries its own cwd/profile/orphan so a caller needs no
	// back-reference to a group (ADR-0002 D2).
	s := sessions[0].(map[string]any)
	if s["cwd"] != g.Cwd {
		t.Errorf("cwd: got %v, want %v", s["cwd"], g.Cwd)
	}
	if s["orphan"] != false {
		t.Errorf("orphan: got %v, want false", s["orphan"])
	}
	if s["profile"] != "" {
		t.Errorf("profile: got %q, want empty for a file agent", s["profile"])
	}
	// Both a byte count (for sort/sum) and a human string are provided.
	if s["fileSize"].(float64) != 2048 {
		t.Errorf("fileSize: got %v, want 2048", s["fileSize"])
	}
	if s["sizeHuman"] != "2.0 KiB" {
		t.Errorf("sizeHuman: got %q, want %q", s["sizeHuman"], "2.0 KiB")
	}
	// modifiedAt is RFC3339 so a caller can parse it without a format guess.
	if _, err := time.Parse(time.RFC3339, s["modifiedAt"].(string)); err != nil {
		t.Errorf("modifiedAt %q is not RFC3339: %v", s["modifiedAt"], err)
	}
}

// assertKeys checks a JSON object's key set exactly — a new field is a
// deliberate contract change and should fail here until the test is updated.
func assertKeys(t *testing.T, what string, obj map[string]any, want ...string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, k := range want {
		wantSet[k] = true
		if _, ok := obj[k]; !ok {
			t.Errorf("%s: missing key %q", what, k)
		}
	}
	for k := range obj {
		if !wantSet[k] {
			t.Errorf("%s: unexpected key %q (contract change? update the test and notify consumers)", what, k)
		}
	}
}

// messageCount must serialize as null (not 0) when unenriched, so a caller can
// tell "not counted yet" from "genuinely empty".
func TestUnenrichedMessageCountIsNull(t *testing.T) {
	s := session("s1", "Kiro", 10, 0)
	s.MessageCount = nil
	b, err := json.Marshal(Flat(nil, []store.Group{liveGroup(t, s)}, ""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc FlatResult
	_ = json.Unmarshal(b, &doc)
	if doc.Sessions[0].MessageCount != nil {
		t.Errorf("messageCount: got %v, want null", *doc.Sessions[0].MessageCount)
	}
	if !strings.Contains(string(b), `"messageCount":null`) {
		t.Errorf("expected messageCount to serialize as null, got %s", b)
	}
}

// Empty results must be [] not null, so a caller can len()/iterate unguarded.
func TestEmptyResultsAreEmptySlicesNotNull(t *testing.T) {
	b, err := json.Marshal(Flat(nil, nil, "0.6.1"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	if doc["sessions"] == nil {
		t.Error("sessions: got null, want []")
	}
	if doc["summary"].(map[string]any)["agentUsage"] == nil {
		t.Error("agentUsage: got null, want []")
	}
}

// Orphaned bytes are reported in orphanSize only — they must not be counted as
// any agent's live usage, since the usage bar shows them as a separate segment.
func TestSummaryExcludesOrphanBytesFromAgentUsage(t *testing.T) {
	live := liveGroup(t, session("live", "Kiro", 1000, 1))
	orphan := store.Group{Cwd: "/definitely/not/a/real/dir", Sessions: []agent.Session{session("dead", "Kiro", 500, 1)}}

	got := summaryOf([]string{"Kiro"}, []store.Group{live, orphan}, "")

	if got.TotalSize != 1500 {
		t.Errorf("totalSize: got %d, want 1500", got.TotalSize)
	}
	if got.OrphanSize != 500 {
		t.Errorf("orphanSize: got %d, want 500", got.OrphanSize)
	}
	if got.OrphanCount != 1 {
		t.Errorf("orphanCount: got %d, want 1", got.OrphanCount)
	}
	if len(got.AgentUsage) != 1 || got.AgentUsage[0].LiveSize != 1000 {
		t.Errorf("agentUsage: got %+v, want one Kiro segment of 1000", got.AgentUsage)
	}
}

// AgentUsage follows installed-agent order (stable colours in the UI), and
// agents with no live bytes get no segment at all.
func TestAgentUsageOrderAndZeroSuppression(t *testing.T) {
	g := liveGroup(t,
		session("a", "Claude Code", 300, 1),
		session("b", "Kiro", 100, 1),
	)
	got := summaryOf([]string{"Kiro", "Claude Code", "Hermes"}, []store.Group{g}, "")

	if len(got.AgentUsage) != 2 {
		t.Fatalf("agentUsage: got %d segments, want 2 (Hermes has no bytes)", len(got.AgentUsage))
	}
	if got.AgentUsage[0].Agent != "Kiro" || got.AgentUsage[1].Agent != "Claude Code" {
		t.Errorf("agentUsage order: got %s,%s — want installed order Kiro,Claude Code",
			got.AgentUsage[0].Agent, got.AgentUsage[1].Agent)
	}
}

// Grouped and Flat must agree on the rollup — they share summaryOf, and the two
// surfaces reporting different totals would be a visible inconsistency.
func TestGroupedAndFlatShareTheSameSummary(t *testing.T) {
	groups := []store.Group{
		liveGroup(t, session("a", "Kiro", 100, 1), session("b", "Kiro", 200, 2)),
		liveGroup(t, session("c", "Claude Code", 300, 3)),
	}
	flat := Flat([]string{"Kiro", "Claude Code"}, groups, "0.6.1")
	grouped := Grouped([]string{"Kiro", "Claude Code"}, groups, "0.6.1")

	if flat.Summary.TotalCount != grouped.TotalCount || flat.Summary.TotalSize != grouped.TotalSize {
		t.Errorf("summaries diverge: flat %+v vs grouped %+v", flat.Summary, grouped.Summary)
	}
	// Flat splices every group's sessions into one list; grouped keeps nesting.
	if len(flat.Sessions) != 3 {
		t.Errorf("flat sessions: got %d, want 3", len(flat.Sessions))
	}
	if len(grouped.Groups) != 2 {
		t.Errorf("grouped groups: got %d, want 2", len(grouped.Groups))
	}
}

// A Hermes group's profile must reach the session view — it distinguishes the
// same cwd under two profiles (ADR-0001 D5).
func TestProfileReachesSessionView(t *testing.T) {
	g := store.Group{Cwd: t.TempDir(), Profile: "x-tec", Sessions: []agent.Session{session("h1", "Hermes", 42, 5)}}
	out := sessionsOf(g)
	if out[0].Profile != "x-tec" {
		t.Errorf("profile: got %q, want %q", out[0].Profile, "x-tec")
	}
	if got := GroupView(g).DisplayName; got == "" {
		t.Error("displayName is empty")
	}
}
