package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Claude interleaves tool results into user-role lines. Those are the agent's own
// loop, so they must not start a new turn — get this wrong and one human
// question splits into dozens of "turns".
func TestClaudeTranscriptGroupsToolResultsIntoTheExchange(t *testing.T) {
	sandboxHome(t)
	path := writeClaudeSession(t, "proj", "s1",
		userLine("s1", "/proj", "first question"),
		`{"type":"assistant","sessionId":"s1","cwd":"/proj","timestamp":"2026-09-08T10:00:01Z","message":{"content":[`+
			`{"type":"thinking","thinking":"pondering"},`+
			`{"type":"text","text":"let me look"},`+
			`{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/y.go"}}]}}`,
		`{"type":"user","sessionId":"s1","timestamp":"2026-09-08T10:00:02Z","message":{"content":[`+
			`{"type":"tool_result","tool_use_id":"t1","content":"file contents here"}]}}`,
		`{"type":"assistant","sessionId":"s1","cwd":"/proj","timestamp":"2026-09-08T10:00:01Z","message":{"content":[{"type":"text","text":"done"}]}}`,
		userLine("s1", "/proj", "second question"),
		`{"type":"assistant","sessionId":"s1","cwd":"/proj","timestamp":"2026-09-08T10:00:01Z","message":{"content":[{"type":"text","text":"ok"}]}}`)
	_ = path

	s := (ClaudeCodeAgent{}).Scan()[0]
	ex, err := (ClaudeCodeAgent{}).Transcript(s, TranscriptOptions{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(ex) != 2 {
		t.Fatalf("got %d turns, want 2 (tool results are not new turns)", len(ex))
	}
	if len(ex[0].Messages) != 4 {
		t.Errorf("first turn: got %d turns, want 4", len(ex[0].Messages))
	}

	asst := ex[0].Messages[1]
	if asst.Text != "let me look" {
		t.Errorf("text: got %q", asst.Text)
	}
	if asst.ThinkingChars == 0 || asst.Thinking != "" {
		t.Errorf("thinking should be folded: chars=%d text=%q", asst.ThinkingChars, asst.Thinking)
	}
	if len(asst.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(asst.Tools))
	}
	tool := asst.Tools[0]
	if tool.Name != "Read" || tool.Summary != "/x/y.go" {
		t.Errorf("tool: got %+v, want Read of /x/y.go", tool)
	}
	// Sizes are reported even when bodies are withheld — that is what makes the
	// folded view useful rather than merely short.
	if tool.OutputBytes != len("file contents here") {
		t.Errorf("outputBytes: got %d", tool.OutputBytes)
	}
	if tool.Output != "" || tool.Args != "" {
		t.Errorf("bodies should be withheld by default: args=%q output=%q", tool.Args, tool.Output)
	}
	if asst.At == nil {
		t.Error("Claude records a timestamp per message; it should be carried through")
	}
}

func TestClaudeTranscriptWithBodies(t *testing.T) {
	sandboxHome(t)
	writeClaudeSession(t, "proj", "s1",
		userLine("s1", "/proj", "q"),
		`{"type":"assistant","sessionId":"s1","cwd":"/proj","timestamp":"2026-09-08T10:00:01Z","message":{"content":[`+
			`{"type":"thinking","thinking":"deliberating"},`+
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","sessionId":"s1","timestamp":"2026-09-08T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"a\nb"}]}}`)

	ex, err := (ClaudeCodeAgent{}).Transcript((ClaudeCodeAgent{}).Scan()[0], TranscriptOptions{Bodies: true})
	if err != nil {
		t.Fatal(err)
	}
	var tool ToolCall
	var thinking string
	for _, e := range ex {
		for _, turn := range e.Messages {
			if len(turn.Tools) > 0 {
				tool = turn.Tools[0]
			}
			if turn.Thinking != "" {
				thinking = turn.Thinking
			}
		}
	}
	if !strings.Contains(tool.Output, "a") || !strings.Contains(tool.Output, "b") {
		t.Errorf("output should be present with bodies: %q", tool.Output)
	}
	if thinking != "deliberating" {
		t.Errorf("thinking text should be present with bodies: %q", thinking)
	}
}

// An image part is a base64 PNG. It must be noted, never inlined.
func TestClaudeTranscriptDoesNotInlineImages(t *testing.T) {
	sandboxHome(t)
	blob := strings.Repeat("A", 5000)
	writeClaudeSession(t, "proj", "s1",
		`{"type":"user","sessionId":"s1","cwd":"/proj","message":{"content":[`+
			`{"type":"text","text":"look at this"},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+blob+`"}}]}}`)

	ex, err := (ClaudeCodeAgent{}).Transcript((ClaudeCodeAgent{}).Scan()[0], TranscriptOptions{Bodies: true})
	if err != nil {
		t.Fatal(err)
	}
	text := ex[0].Messages[0].Text
	if strings.Contains(text, blob[:100]) {
		t.Error("base64 image data was inlined into the transcript")
	}
	if !strings.Contains(text, "[image]") {
		t.Errorf("an image should be noted, got %q", text)
	}
}

// Kiro pairs a toolUse with a toolResult that arrives in a later event, so the
// pairing has to survive the whole stream rather than one line.
func TestKiroTranscriptPairsToolsAcrossEvents(t *testing.T) {
	sandboxHome(t)
	events := strings.Join([]string{
		`{"kind":"Prompt","data":{"content":[{"kind":"text","data":"do the thing"}]}}`,
		`{"kind":"AssistantMessage","data":{"content":[` +
			`{"kind":"thinking","data":"considering"},` +
			`{"kind":"text","data":"reading first"},` +
			`{"kind":"toolUse","data":{"toolUseId":"u1","name":"read","input":{"operations":[{"path":"/a/b.md"}]}}}]}}`,
		`{"kind":"ToolResults","data":{"content":[{"kind":"toolResult","data":{"toolUseId":"u1","content":[{"kind":"text","data":"the file body"}]}}]}}`,
		`{"kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"finished"}]}}`,
	}, "\n") + "\n"
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/proj", "title": "t"},
		map[string]string{".jsonl": events})

	ex, err := (KiroAgent{}).Transcript((KiroAgent{}).Scan()[0], TranscriptOptions{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(ex) != 1 {
		t.Fatalf("got %d turns, want 1", len(ex))
	}
	turns := ex[0].Messages
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3 (ToolResults is not a turn)", len(turns))
	}
	tool := turns[1].Tools[0]
	if tool.Name != "read" || tool.Summary != "/a/b.md" {
		t.Errorf("tool: got %+v", tool)
	}
	if tool.OutputBytes != len("the file body") {
		t.Errorf("the result did not reach its call: outputBytes=%d", tool.OutputBytes)
	}
	// Kiro records no per-message time.
	if turns[0].At != nil {
		t.Errorf("Kiro has no per-message timestamp, got %v", *turns[0].At)
	}
}

func TestKiroTranscriptWithoutAConversationFileErrors(t *testing.T) {
	sandboxHome(t)
	writeKiroBundle(t, "sid1", map[string]any{"cwd": "/proj"}, nil)
	if _, err := (KiroAgent{}).Transcript((KiroAgent{}).Scan()[0], TranscriptOptions{}); err == nil {
		t.Error("expected an error for a bundle with no .jsonl")
	}
}

// Codex keeps tool calls as their own top-level items, so they have to be
// attached to the assistant turn they followed. Its `developer` messages are
// injected instructions and its reasoning is encrypted.
func TestCodexTranscriptAttachesToolsAndSkipsDeveloper(t *testing.T) {
	home := codexSandbox(t)
	body := strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>"}]}}`,
		`{"type":"response_item","timestamp":"2026-09-08T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"real question"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working on it"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"go build ./...\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}`,
		`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"rsn_opaque","summary":[]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n injected"}]}}`,
	}, "\n") + "\n"
	newCodexDB(t, home, 5, []codexRow{
		{id: "s1", cwd: "/proj", title: "t", source: `"cli"`, updatedMs: 1,
			rollout: rolloutPath(home, "26", "s1"), body: body},
	})

	ex, err := (CodexAgent{}).Transcript((CodexAgent{}).Scan()[0], TranscriptOptions{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	// The developer row is not conversation, and the injected wrapper does not
	// start an turn — so everything belongs to the one real question.
	if len(ex) != 1 {
		t.Fatalf("got %d turns, want 1", len(ex))
	}
	var tool *ToolCall
	roles := []string{}
	for _, turn := range ex[0].Messages {
		roles = append(roles, turn.Role)
		if len(turn.Tools) > 0 {
			tool = &turn.Tools[0]
		}
		if turn.ThinkingChars != 0 {
			t.Errorf("Codex reasoning is encrypted; nothing should be reported: %d", turn.ThinkingChars)
		}
	}
	for _, r := range roles {
		if r == "developer" {
			t.Error("a developer message reached the transcript")
		}
	}
	if tool == nil {
		t.Fatal("the function_call was not attached to a turn")
	}
	if tool.Name != "exec_command" || tool.Summary != "go build ./..." {
		t.Errorf("tool: got %+v", tool)
	}
	if tool.OutputBytes != len("ok") {
		t.Errorf("outputBytes: got %d, want 2", tool.OutputBytes)
	}
	if ex[0].Messages[0].At == nil {
		t.Error("Codex records a line timestamp; it should be carried through")
	}
}

func TestCodexTranscriptWithoutARolloutErrors(t *testing.T) {
	home := codexSandbox(t)
	newCodexDB(t, home, 5, []codexRow{{id: "s1", cwd: "/x", title: "t", source: `"cli"`, updatedMs: 1}})
	if _, err := (CodexAgent{}).Transcript((CodexAgent{}).Scan()[0], TranscriptOptions{}); err == nil {
		t.Error("expected an error for a session with no rollout file")
	}
}

// Hermes stores a tool's result as its own row, which belongs to the assistant
// turn that called it rather than being a turn of its own.
func TestHermesTranscriptFoldsToolRows(t *testing.T) {
	home := hermesSandbox(t)
	dbPath := filepath.Join(home, "state.db")
	newHermesDB(t, dbPath, []hermesRow{
		{id: "s1", source: "cli", cwd: "/proj", startedAt: 1, contents: []string{"a question", "an answer"}},
	}, nil)
	// tool_calls on the assistant row, and the result as a role='tool' row.
	execHermes(t, dbPath,
		`UPDATE messages SET tool_calls = '[{"function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]'
		 WHERE session_id='s1' AND role='assistant'`)
	execHermes(t, dbPath,
		`INSERT INTO messages (session_id, role, content, tool_name) VALUES ('s1','tool','listing output','terminal')`)

	ex, err := (HermesAgent{}).Transcript((HermesAgent{}).Scan()[0], TranscriptOptions{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(ex) != 1 {
		t.Fatalf("got %d turns, want 1", len(ex))
	}
	turns := ex[0].Messages
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (the tool row folds into the assistant turn)", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Text != "a question" {
		t.Errorf("first turn: %+v", turns[0])
	}
	if len(turns[1].Tools) != 1 {
		t.Fatalf("the assistant turn has %d tools, want 1", len(turns[1].Tools))
	}
	tool := turns[1].Tools[0]
	if tool.Name != "terminal" || tool.Summary != "ls" {
		t.Errorf("tool: got %+v", tool)
	}
	if tool.OutputBytes != len("listing output") {
		t.Errorf("the tool row did not reach its call: outputBytes=%d", tool.OutputBytes)
	}
}

// A malformed tool_calls column yields one opaque entry rather than silently
// dropping the fact that a tool ran.
func TestHermesToolCallsFallsBackOnUnknownShapes(t *testing.T) {
	got := hermesToolCalls(`not json at all`, false)
	if len(got) != 1 || got[0].Name != "tool" {
		t.Errorf("got %+v, want one opaque entry", got)
	}
	valid := hermesToolCalls(`[{"function":{"name":"grep","arguments":"{\"pattern\":\"x\"}"}}]`, true)
	if len(valid) != 1 || valid[0].Name != "grep" || valid[0].Args == "" {
		t.Errorf("got %+v", valid)
	}
}

func execHermes(t *testing.T, dbPath, stmt string) {
	t.Helper()
	db, err := openRW(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

var _ = os.Getenv // keep the os import honest across build variations
