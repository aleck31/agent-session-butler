package agent

import (
	"encoding/json"
	"testing"
)

// A diagnostic against the real machine, like the scan benchmarks: it reports
// what the default window actually costs on this history. Only runs when asked.
//
//	go test ./internal/agent/ -run ProbeTranscript -v -count=1
func TestProbeTranscriptSize(t *testing.T) {
	if testing.Short() {
		t.Skip("diagnostic; needs real session data")
	}
	readers := map[string]struct {
		a Agent
		r Reader
	}{
		"Kiro": {KiroAgent{}, KiroAgent{}},
	}
	for name, pair := range readers {
		if !pair.a.Installed() {
			continue
		}
		n := 0
		for _, s := range pair.a.Scan() {
			if s.FileSize < 5_000_000 || n >= 3 {
				continue
			}
			n++
			ex, err := pair.r.Transcript(s, TranscriptOptions{Tail: DefaultTurns})
			if err != nil {
				t.Logf("%s %s: %v", name, s.ID[:8], err)
				continue
			}
			b, _ := json.Marshal(ex)
			turns, tools := 0, 0
			for _, e := range ex {
				turns += len(e.Messages)
				for _, tn := range e.Messages {
					tools += len(tn.Tools)
				}
			}
			t.Logf("%s %s  %5.1fMB → %d turns, %d turns, %d tools, JSON %.1f KB (%.4f%% of session)",
				name, s.ID[:8], float64(s.FileSize)/1048576, len(ex), turns, tools,
				float64(len(b))/1024, 100*float64(len(b))/float64(s.FileSize))
		}
	}
}
