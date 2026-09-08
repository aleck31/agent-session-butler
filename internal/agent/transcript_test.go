package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// An turn is one human-agent round, not one message. A single turn holds
// 3 to 530 messages in real sessions, so counting messages produces a window
// nobody can predict — this is the property the whole model rests on.
func TestGroupExchangesSplitsOnUserInput(t *testing.T) {
	turns := []Message{
		{Role: "user"},      // 0 real input
		{Role: "assistant"}, // 1
		{Role: "user"},      // 2 tool result, not a person
		{Role: "assistant"}, // 3
		{Role: "user"},      // 4 real input
		{Role: "assistant"}, // 5
	}
	real := map[int]bool{0: true, 4: true}

	ex := groupTurns(turns, func(i int) bool { return real[i] })
	if len(ex) != 2 {
		t.Fatalf("got %d turns, want 2", len(ex))
	}
	if len(ex[0].Messages) != 4 {
		t.Errorf("first turn: got %d turns, want 4 (the tool-result user turn belongs to it)", len(ex[0].Messages))
	}
	if len(ex[1].Messages) != 2 {
		t.Errorf("second turn: got %d turns, want 2", len(ex[1].Messages))
	}
}

// A transcript that opens with agent output rather than a user message still
// produces one turn instead of dropping the turns.
func TestGroupExchangesWithNoLeadingUserTurn(t *testing.T) {
	turns := []Message{{Role: "assistant"}, {Role: "assistant"}}
	ex := groupTurns(turns, func(int) bool { return false })
	if len(ex) != 1 || len(ex[0].Messages) != 2 {
		t.Errorf("got %+v, want one turn of two turns", ex)
	}
	if got := groupTurns(nil, func(int) bool { return false }); len(got) != 0 {
		t.Errorf("empty input: got %d turns, want 0", len(got))
	}
}

func TestWindowExchanges(t *testing.T) {
	mk := func(n int) []Turn {
		out := make([]Turn, n)
		for i := range out {
			out[i] = Turn{Messages: []Message{{Text: string(rune('a' + i))}}}
		}
		return out
	}
	first := func(ex []Turn) string { return ex[0].Messages[0].Text }
	last := func(ex []Turn) string { return ex[len(ex)-1].Messages[0].Text }

	all := mk(5)
	if got := windowTurns(all, TranscriptOptions{Tail: 2}); len(got) != 2 || last(got) != "e" {
		t.Errorf("tail 2: got %d ending %q", len(got), last(got))
	}
	if got := windowTurns(all, TranscriptOptions{Head: 2}); len(got) != 2 || first(got) != "a" {
		t.Errorf("head 2: got %d starting %q", len(got), first(got))
	}
	// No bound means everything: the caller has opted into the cost.
	if got := windowTurns(all, TranscriptOptions{}); len(got) != 5 {
		t.Errorf("unbounded: got %d, want 5", len(got))
	}
	// A window larger than the transcript is not an error.
	if got := windowTurns(all, TranscriptOptions{Tail: 99}); len(got) != 5 {
		t.Errorf("oversized tail: got %d, want 5", len(got))
	}
	// Head wins when both are set, matching the CLI where the last flag clears the other.
	if got := windowTurns(all, TranscriptOptions{Head: 1, Tail: 3}); len(got) != 1 || first(got) != "a" {
		t.Errorf("head and tail: got %d starting %q", len(got), first(got))
	}
}

// Reasoning is folded like tool output: a length by default, text only when asked.
func TestSetThinkingFoldsByDefault(t *testing.T) {
	var t1 Message
	t1.setThinking("  三个字  ", false)
	if t1.ThinkingChars != 3 {
		t.Errorf("chars: got %d, want 3 (runes, trimmed)", t1.ThinkingChars)
	}
	if t1.Thinking != "" {
		t.Errorf("text should be withheld without bodies, got %q", t1.Thinking)
	}

	var t2 Message
	t2.setThinking("first", true)
	t2.setThinking("second", true)
	if t2.ThinkingChars != 11 {
		t.Errorf("chars accumulate: got %d, want 11", t2.ThinkingChars)
	}
	if !strings.Contains(t2.Thinking, "first") || !strings.Contains(t2.Thinking, "second") {
		t.Errorf("both blocks should be kept, got %q", t2.Thinking)
	}

	// Empty reasoning is not reasoning: Claude sometimes emits a signature with no text.
	var t3 Message
	t3.setThinking("   ", true)
	if t3.ThinkingChars != 0 || t3.Thinking != "" {
		t.Errorf("blank thinking should be ignored, got %d/%q", t3.ThinkingChars, t3.Thinking)
	}
}

// The one-line summary has to name the file or command a tool acted on. Agents
// bury it under bookkeeping keys, and Kiro's edit tools put only an operation
// name at the top level.
func TestSummariseArgs(t *testing.T) {
	for name, tc := range map[string]struct{ args, want string }{
		"kiro edit puts the path one level down": {
			`{"__tool_use_purpose":"fix a typo","command":"strReplace","operations":[{"path":"/tmp/a.md"}]}`,
			"/tmp/a.md"},
		"a shell command is used as-is": {
			`{"command":"ls -la /tmp"}`, "ls -la /tmp"},
		"an operation name alone is not informative": {
			`{"command":"strReplace","__tool_use_purpose":"tidy up"}`, "tidy up"},
		"file_path":                    {`{"file_path":"/x/y.go","content":"..."}`, "/x/y.go"},
		"codex cmd":                    {`{"cmd":"go test ./..."}`, "go test ./..."},
		"pattern":                      {`{"pattern":"func main"}`, "func main"},
		"a short real command is kept": {`{"command":"ls"}`, "ls"},
		"go build is kept":             {`{"command":"go build"}`, "go build"},
		"first line only":              {`{"command":"line one\nline two"}`, "line one"},
		"not json falls back":          {`just a string`, "just a string"},
		"unknown keys":                 {`{"weird":1}`, `{"weird":1}`},
	} {
		if got := summariseArgs(tc.args, 70); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestSummariseClips(t *testing.T) {
	long := strings.Repeat("字", 200)
	got := summarise(long, 70)
	if n := len([]rune(got)); n != 71 { // 70 plus the ellipsis
		t.Errorf("got %d runes, want 71", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want an ellipsis", got)
	}
}

// Bodies are decoded and bounded: Hermes stores \uXXXX escapes, and tool output
// runs to megabytes even when the caller did ask for it.
func TestReadableBodyDecodesAndBounds(t *testing.T) {
	// What Hermes actually stores: the characters as \uXXXX escape sequences.
	escaped := `{"note":"\u4e2d\u6587"}`
	got := readableBody(escaped)
	if !strings.Contains(got, "中文") {
		t.Errorf("escapes should be decoded, got %q", got)
	}
	if strings.Contains(got, `\u4e2d`) {
		t.Errorf("raw escapes survived: %q", got)
	}

	huge, _ := json.Marshal(map[string]string{"blob": strings.Repeat("x", 40_000)})
	clipped := readableBody(string(huge))
	if len(clipped) > maxBodyBytes+64 {
		t.Errorf("body not bounded: %d bytes", len(clipped))
	}
	if !strings.Contains(clipped, "more bytes") {
		t.Error("a clipped body should say how much was dropped")
	}
}

// Clipping must not split a multi-byte character.
func TestClipBytesRespectsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("字", 100) // 3 bytes each
	for _, limit := range []int{10, 11, 12, 50, 299} {
		got := clipBytes(s, limit)
		head := strings.SplitN(got, "\n", 2)[0]
		if !utf8Valid(head) {
			t.Errorf("limit %d produced invalid UTF-8: %q", limit, head)
		}
	}
	// Under the limit, the string is returned untouched.
	if got := clipBytes("short", 100); got != "short" {
		t.Errorf("got %q, want it unchanged", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
