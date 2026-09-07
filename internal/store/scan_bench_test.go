package store

import "testing"

// BenchmarkStoreScan measures the whole scan path against the real HOME:
// concurrent per-agent scans plus the cache merge, prune and grouping the
// per-agent benchmarks in internal/agent leave out. Together they say whether a
// slow `list` is an agent's fault or the store's.
//
// Read-only, and only runs under -bench — see the note in
// internal/agent/scan_bench_test.go about using real data here.
//
//	go test ./internal/store/ -bench Scan -benchtime 5x -run '^$'
func BenchmarkStoreScan(b *testing.B) {
	s := New()
	if len(s.InstalledAgents()) == 0 {
		b.Skip("no agents installed on this machine")
	}
	var groups, sessions int
	for b.Loop() {
		gs := s.Scan()
		groups, sessions = len(gs), 0
		for _, g := range gs {
			sessions += len(g.Sessions)
		}
	}
	b.ReportMetric(float64(groups), "groups")
	b.ReportMetric(float64(sessions), "sessions")
}

// BenchmarkStoreScanCached measures a re-scan with a warm mtime cache — the case
// the web UI hits on every refresh.
func BenchmarkStoreScanCached(b *testing.B) {
	s := New()
	if len(s.InstalledAgents()) == 0 {
		b.Skip("no agents installed on this machine")
	}
	for _, g := range s.Scan() {
		s.EnrichGroup(g)
	}
	for b.Loop() {
		s.Scan()
	}
}
