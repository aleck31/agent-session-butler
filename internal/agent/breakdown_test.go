package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

// want is a Breakdown with thinking -1 meaning "not recorded by this store".
type want struct{ user, agent, tools, thinking int }

func checkBreakdown(t *testing.T, s Session, w want) {
	t.Helper()
	b := s.Breakdown
	if b == nil || b.User == nil || b.Agent == nil || b.ToolCalls == nil {
		t.Fatalf("breakdown = %+v, want user, agent and toolCalls present", b)
	}
	if *b.User != w.user || *b.Agent != w.agent || *b.ToolCalls != w.tools {
		t.Errorf("user/agent/tools = %d/%d/%d, want %d/%d/%d", *b.User, *b.Agent, *b.ToolCalls, w.user, w.agent, w.tools)
	}
	switch {
	case w.thinking < 0 && b.Thinking != nil:
		t.Errorf("thinking = %d for a store that records none; want it absent", *b.Thinking)
	case w.thinking >= 0 && (b.Thinking == nil || *b.Thinking != w.thinking):
		t.Errorf("thinking = %v, want %d", b.Thinking, w.thinking)
	}
	// The total is the same quantity for every store: a person's messages plus the agent's replies.
	if s.MessageCount == nil || *s.MessageCount != w.user+w.agent {
		t.Errorf("messageCount = %v, want user+agent = %d", s.MessageCount, w.user+w.agent)
	}
}

// Claude writes one line per content block and returns tool results as user lines; counting lines
// made a session with 202 prompts and 1322 replies report 3821 messages.
func TestClaudeBreakdown(t *testing.T) {
	sandboxHome(t)
	writeClaudeSession(t, "-p", "s",
		`{"type":"user","cwd":"/p","sessionId":"s","message":{"role":"user","content":"fix the bug"}}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"thinking","thinking":"hmm"}]}}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"id":"msg_2","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"thanks"}]}}`,
	)
	got := (ClaudeCodeAgent{}).Scan()
	if len(got) != 1 {
		t.Fatalf("scanned %d sessions", len(got))
	}
	checkBreakdown(t, (ClaudeCodeAgent{}).Enrich(got[0]), want{user: 2, agent: 2, tools: 1, thinking: 1})
}

func TestKiroV2Breakdown(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "k1", map[string]any{"session_id": "k1", "cwd": "/p", "title": "t"}, map[string]string{".jsonl": strings.Join([]string{
		`{"version":"v1","kind":"Prompt","data":{"content":[{"kind":"text","data":"go"}]}}`,
		`{"version":"v1","kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"x"},{"kind":"thinking","data":"y"},{"kind":"toolUse","data":{"toolUseId":"t","name":"n","input":{}}}]}}`,
		`{"version":"v1","kind":"ToolResults","data":{"content":[{"kind":"toolResult","data":{"toolUseId":"t"}}]}}`,
		`{"version":"v1","kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"done"}]}}`,
	}, "\n")})
	got := (KiroAgent{}).scanV2()
	if len(got) != 1 {
		t.Fatalf("scanned %d sessions", len(got))
	}
	checkBreakdown(t, (KiroAgent{}).Enrich(got[0]), want{user: 1, agent: 2, tools: 1, thinking: 1})
}

func TestKiroV1Breakdown(t *testing.T) {
	blob := `{"history":[` +
		`{"user":{"content":{"Prompt":{"prompt":"go"}}},"assistant":{"ToolUse":{"content":"","tool_uses":[{"id":"a"},{"id":"b"}]}}},` +
		`{"user":{"content":{"ToolUseResults":{}}},"assistant":{"Response":{"content":"done"}}}]}`
	got, _ := kiroV1Count([]byte(blob))
	checkBreakdown(t, got.apply(Session{}), want{user: 1, agent: 2, tools: 2, thinking: -1})
}

// v3 writes a row per text segment and per tool call; one model response is still one reply.
func TestKiroV3Breakdown(t *testing.T) {
	home := v3Sandbox(t)
	s, _ := (KiroAgent{}).parseV3(writeV3(t, home, "h", "sess_x", "/p", "t", v3Rows...))
	checkBreakdown(t, (KiroAgent{}).Enrich(s), want{user: 2, agent: 2, tools: 1, thinking: -1})
}

func TestCodexBreakdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"x"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"ok"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
	}, "\n")+"\n")
	checkBreakdown(t, (CodexAgent{}).Enrich(Session{FilePaths: []string{path}}), want{user: 1, agent: 1, tools: 1, thinking: 1})
}

func TestHermesBreakdown(t *testing.T) {
	home := hermesSandbox(t)
	newHermesDB(t, filepath.Join(home, "state.db"), []hermesRow{
		{id: "a", source: "cli", cwd: "/p", startedAt: 1000, contents: []string{"q", "a", "q2"}, toolRows: []string{"r1", "r2"}},
	}, nil)
	got := (HermesAgent{}).Scan()
	if len(got) != 1 {
		t.Fatalf("scanned %d sessions", len(got))
	}
	checkBreakdown(t, got[0], want{user: 2, agent: 1, tools: 2, thinking: -1})
}
