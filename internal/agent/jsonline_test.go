package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reader replaces a full decode, so each case is one a naive byte search gets wrong.
func TestTopLevelString(t *testing.T) {
	for name, tc := range map[string]struct {
		line, key, want string
		ok              bool
	}{
		"first key":                {`{"kind":"Prompt","data":{}}`, "kind", "Prompt", true},
		"after a nested object":    {`{"data":{"kind":"inner"},"kind":"outer"}`, "kind", "outer", true},
		"after a nested array":     {`{"a":[{"kind":"x"},"kind"],"kind":"y"}`, "kind", "y", true},
		"key text inside a string": {`{"note":"\"kind\":\"fake\"","kind":"real"}`, "kind", "real", true},
		"key as a string value":    {`{"name":"kind","kind":"real"}`, "kind", "real", true},
		"only nested":              {`{"data":{"kind":"inner"}}`, "kind", "", false},
		"absent":                   {`{"type":"user"}`, "kind", "", false},
		"not a string":             {`{"kind":3}`, "kind", "", false},
		"escaped value":            {`{"t":"a\"bé"}`, "t", `a"bé`, true},
		"whitespace":               {`{ "kind" : "Prompt" }`, "kind", "Prompt", true},
		"cut off before the key":   {`{"data":{"x":1`, "kind", "", false},
		"cut off after the value":  {`{"kind":"Prompt","data":{"x":`, "kind", "Prompt", true},
		"not an object":            {`["kind","Prompt"]`, "kind", "", false},
		"empty":                    {``, "kind", "", false},
	} {
		got, ok := topLevelString([]byte(tc.line), tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: topLevelString(%s, %q) = %q, %v; want %q, %v", name, tc.line, tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

// Codex filters on payload.type, read by handing fieldStart's offset back to topLevelString.
func TestFieldStartScopesANestedLookup(t *testing.T) {
	line := []byte(`{"type":"response_item","payload":{"role":"user","type":"message"},"type2":"x"}`)
	j, ok := fieldStart(line, "payload")
	if !ok {
		t.Fatal("payload not found")
	}
	if got, _ := topLevelString(line[j:], "type"); got != "message" {
		t.Errorf("payload.type = %q, want message", got)
	}
	// The scoped lookup must not escape the payload object into the outer one.
	if _, ok := topLevelString(line[j:], "type2"); ok {
		t.Error("lookup inside payload found a key of the enclosing object")
	}
}

// A partly written last line is what the full decode used to reject; the counts must not move.
func TestKiroEnrichIgnoresAnUnfinishedLineAndKeyOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"version":"v1","kind":"Prompt","data":{}}`,
		`{"data":{"kind":"Prompt"},"kind":"AssistantMessage"}`, // kind after a nested decoy
		`{"version":"v1","kind":"ToolResults","data":{"content":"` + strings.Repeat("x", 1<<20) + `"}}`,
		`{"version":"v1","kind":"Prompt","data":{"content":"still bei`, // being written
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	got := KiroAgent{}.Enrich(Session{FilePaths: []string{path}})
	if got.MessageCount == nil || *got.MessageCount != 2 {
		t.Errorf("message count = %v, want 2", got.MessageCount)
	}
}

// v1 is counted by walking the blob, so the cases are the ones a decode handled for free.
func TestKiroV1CountWalk(t *testing.T) {
	prompt := func(text string) string {
		return `{"user":{"content":{"Prompt":{"prompt":"` + text + `"}}},"assistant":{"Response":{"content":"a"}}}`
	}
	for name, tc := range map[string]struct {
		blob, title string
		count       int
	}{
		"plain":        {`{"history":[` + prompt("first") + `,` + prompt("second") + `]}`, "first", 4},
		"tool results": {`{"history":[{"user":{"content":{"ToolUseResults":{"x":1}}},"assistant":{"Response":{}}}]}`, "", 1},
		// "Prompt" as text inside tool output, or as a key nested deeper, is not a prompt.
		"decoys":                   {`{"history":[{"user":{"content":{"ToolUseResults":{"text":"\"Prompt\":{}","Prompt":1}}},"assistant":{}}]}`, "", 0},
		"empty or null assistant":  {`{"history":[{"user":{"content":{}},"assistant":{}},{"assistant":null}]}`, "", 0},
		"assistant before user":    {`{"history":[{"assistant":{"Response":{}},"user":{"content":{"Prompt":{"prompt":"q"}}}}]}`, "q", 2},
		"history after other keys": {`{"conversation_id":"x","context":{"history":[1]},"history":[` + prompt("real") + `]}`, "real", 2},
		"first prompt empty":       {`{"history":[` + prompt("") + `,` + prompt("later") + `]}`, "later", 4},
		"no history":               {`{"conversation_id":"x"}`, "", 0},
		"cut off mid-history":      {`{"history":[` + prompt("q") + `,{"user":{"content":{"Pro`, "q", 2},
		"garbage between elements": {`{"history":[` + prompt("q") + ` x ` + prompt("r") + `]}`, "q", 2},
	} {
		count, title := kiroV1Count([]byte(tc.blob))
		if count != tc.count || title != tc.title {
			t.Errorf("%s: kiroV1Count = (%d, %q), want (%d, %q)", name, count, title, tc.count, tc.title)
		}
	}
}
