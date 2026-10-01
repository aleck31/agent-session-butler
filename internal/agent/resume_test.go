package agent

import (
	"slices"
	"testing"
)

// The Kiro rules took the consumer several attempts; each case is one of those attempts.
func TestKiroResumeArgv(t *testing.T) {
	for name, tc := range map[string]struct {
		store  string
		shared bool
		want   []string
	}{
		"v2":                {kiroStoreV2, false, []string{"kiro-cli", "chat", "--resume-id", "x"}},
		"v2 shared":         {kiroStoreV2, true, []string{"kiro-cli", "chat", "--resume-id", "x"}},
		"v1 alone":          {kiroStoreV1, false, []string{"kiro-cli", "chat", "--resume-id", "x"}},
		"v1 with a v2 copy": {kiroStoreV1, true, []string{"kiro-cli", "chat", "--resume-id", "x", "--agent-engine", "v1"}},
	} {
		if got := (KiroAgent{}).ResumeArgv(Session{ID: "x", Store: tc.store}, tc.shared); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// Hermes has no resume path asbutler can vouch for across profiles, so it must not claim one.
func TestHermesIsNotAResumer(t *testing.T) {
	if _, ok := any(HermesAgent{}).(Resumer); ok {
		t.Error("HermesAgent implements Resumer; a non-default profile's resume is unverified")
	}
}
