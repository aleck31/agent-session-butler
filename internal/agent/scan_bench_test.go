package agent

import (
	"testing"
)

// These are diagnostics, not regression tests, and they are the one place in
// this package that reads the real HOME rather than a sandbox — deliberately,
// because the question they answer is "where does a `list` query spend its time
// on a machine with real history", which synthetic fixtures cannot tell you.
// Safe to do so: Scan and Installed never write, and benchmarks only run under
// -bench, so `go test ./...` never touches real session data.
//
// A per-agent breakdown is the point. It is what showed Kiro at 494 ms against
// Claude Code's 22 ms, redirecting an optimization that had been aimed at the
// wrong agent. See internal/store for the end-to-end counterpart.
//
//	go test ./internal/agent/ -bench Scan -benchtime 5x -run '^$'
func BenchmarkScanPerAgent(b *testing.B) {
	agents := []Agent{
		KiroAgent{},
		ClaudeCodeAgent{},
		CodexAgent{},
		HermesAgent{},
	}
	for _, a := range agents {
		if !a.Installed() {
			b.Run(a.Name()+"(not installed)", func(b *testing.B) { b.Skip("not installed") })
			continue
		}
		b.Run(a.Name(), func(b *testing.B) {
			var n int
			for b.Loop() {
				n = len(a.Scan())
			}
			b.ReportMetric(float64(n), "sessions")
		})
	}
}

// BenchmarkInstalledPerAgent measures discovery alone — the probe that decides
// whether an agent is present. It runs on every invocation, including ones that
// end up returning nothing.
func BenchmarkInstalledPerAgent(b *testing.B) {
	for _, a := range []Agent{KiroAgent{}, ClaudeCodeAgent{}, CodexAgent{}, HermesAgent{}} {
		b.Run(a.Name(), func(b *testing.B) {
			for b.Loop() {
				a.Installed()
			}
		})
	}
}
