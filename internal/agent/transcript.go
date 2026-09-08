package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultTurns is how many turns a transcript returns when the caller does not
// say. Measured against real sessions: the last 5 turns are 2.6–77 KB of text out
// of 22–39 MB of session. See ADR-0005.
const DefaultTurns = 5

// toolSummaryRunes bounds the one-line summary of a tool's arguments.
const toolSummaryRunes = 70

// Turn is one round of human-agent interaction: the user's message and
// everything the agent did before the user spoke again. This is the unit a
// reader thinks in — a single turn can hold anywhere from 3 to 530 messages,
// so counting messages produces a window nobody can predict.
type Turn struct {
	Messages []Message `json:"messages"`
}

// Message is one message inside a turn, normalised across agents. Every
// backend stores conversations differently; this is the shape they all reduce
// to. See ADR-0005 for what each one can and cannot supply.
type Message struct {
	Role string `json:"role"` // "user", "assistant", or "system"
	Text string `json:"text"` // empty when a turn is only tool activity
	// ThinkingChars is the length of the agent's reasoning; the text itself is
	// folded like tool output and only included when bodies are requested. Zero
	// for agents that store no readable reasoning (Codex encrypts it, Hermes
	// leaves the columns empty).
	ThinkingChars int        `json:"thinkingChars,omitempty"`
	Thinking      string     `json:"thinking,omitempty"`
	Tools         []ToolCall `json:"tools,omitempty"`
	At            *string    `json:"at,omitempty"` // RFC3339; nil when the agent stores no per-message time
}

// setThinking records reasoning as a length, keeping the text only when asked.
func (t *Message) setThinking(text string, bodies bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	t.ThinkingChars += len([]rune(text))
	if bodies {
		t.Thinking = appendBlock(t.Thinking, text)
	}
}

// ToolCall is one tool invocation and its result. Args and Output are populated
// only when the caller asks for bodies: tool traffic is the bulk of a session's
// bytes, so summaries are the default on every surface.
type ToolCall struct {
	Name        string `json:"name"`
	Summary     string `json:"summary"`     // one line: the gist of the arguments
	ArgsBytes   int    `json:"argsBytes"`   // size of the full arguments
	OutputBytes int    `json:"outputBytes"` // size of the full result
	Args        string `json:"args,omitempty"`
	Output      string `json:"output,omitempty"`
}

// TranscriptOptions selects a slice of a session and how much of it to load.
// Sessions reach 97 MB, so a reader must never assume it can hold one whole.
type TranscriptOptions struct {
	// Tail and Head count turns, not messages. Zero means unset; when both
	// are zero the caller wants everything and has opted into the cost.
	Tail int
	Head int
	// Bodies includes full tool arguments and output.
	Bodies bool
}

// Reader is implemented by agents that can render a session's conversation.
// Kept separate from Agent so a new backend can be listed and cleaned up before
// anyone works out how to parse its transcript format.
type Reader interface {
	Transcript(s Session, opts TranscriptOptions) ([]Turn, error)
}

// groupTurns splits a flat turn list wherever the user speaks. A user turn
// that only carries a tool result is the agent's own loop, not a person typing,
// so callers mark real user input with isUserInput.
func groupTurns(turns []Message, isUserInput func(i int) bool) []Turn {
	var out []Turn
	var cur []Message
	for i, t := range turns {
		if isUserInput(i) && len(cur) > 0 {
			out = append(out, Turn{Messages: cur})
			cur = nil
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		out = append(out, Turn{Messages: cur})
	}
	return out
}

// windowTurns applies Head/Tail. Runs last, so a reader is free to have
// discarded tool bodies while streaming.
func windowTurns(ts []Turn, opts TranscriptOptions) []Turn {
	switch {
	case opts.Head > 0 && opts.Head < len(ts):
		return ts[:opts.Head]
	case opts.Tail > 0 && opts.Tail < len(ts):
		return ts[len(ts)-opts.Tail:]
	}
	return ts
}

// maxBodyBytes bounds one tool body even when bodies are requested. Tool output
// runs to megabytes; --tools means "show me what it did", not "dump the session".
const maxBodyBytes = 8 << 10

// readableBody prepares a tool body for display: JSON is re-marshalled so
// \uXXXX escapes become the characters they stand for, and everything is clipped.
func readableBody(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			raw = string(pretty)
		}
	}
	return clipBytes(raw, maxBodyBytes)
}

// clipBytes truncates on a rune boundary and says how much was dropped.
func clipBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… (%d more bytes)", len(s)-cut)
}

// summariseArgs turns a tool's JSON arguments into one readable line. Agents wrap
// the interesting value (a path, a command) in a JSON object alongside bookkeeping
// keys, so the known carriers are preferred over dumping the raw object.
func summariseArgs(raw string, limit int) string {
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return summarise(raw, limit)
	}
	// Nested paths first: Kiro's edit tools carry only an operation name at the top
	// level ("strReplace"), with the file it acts on one level down.
	if ops, ok := obj["operations"].([]any); ok {
		for _, o := range ops {
			m, ok := o.(map[string]any)
			if !ok {
				continue
			}
			if p, ok := m["path"].(string); ok && p != "" {
				return summarise(p, limit)
			}
		}
	}
	for _, key := range []string{"cmd", "command", "file_path", "path", "pattern", "query", "url", "description"} {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			// Kiro puts an operation name in `command` ("strReplace", "fsWrite").
			// Skip those; a real command like "ls" or "go build" is informative.
			if key == "command" && isOperationName(v) {
				continue
			}
			return summarise(v, limit)
		}
	}
	// Last resort: any short string value beats dumping the whole object.
	for _, key := range []string{"__tool_use_purpose", "prompt", "content"} {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			return summarise(v, limit)
		}
	}
	return summarise(raw, limit)
}

// isOperationName reports whether a value looks like a camelCase operation label
// rather than a command someone would run: one token, with an inner capital.
func isOperationName(v string) bool {
	if strings.ContainsAny(v, " \t/-") {
		return false
	}
	for _, r := range v[1:] {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

// summarise reduces a string to one clipped line for the folded view.
func summarise(s string, limit int) string {
	line := s
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		line = s[:i]
	}
	line = strings.TrimSpace(line)
	if len([]rune(line)) > limit {
		return string([]rune(line)[:limit]) + "…"
	}
	return line
}
