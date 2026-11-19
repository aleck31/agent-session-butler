package agent

import (
	"errors"
	"testing"
)

// The listing's relocatable field and mv/cp must agree, so a refusal is the same error from both (issue #6).
func TestRelocateRefusalIsWhatRelocateReturns(t *testing.T) {
	for name, tc := range map[string]struct {
		a interface {
			Agent
			RelocateChecker
		}
		s      Session
		refuse bool
	}{
		"Kiro v1":     {KiroAgent{}, Session{ID: "x", Store: kiroStoreV1}, true},
		"Kiro v2":     {KiroAgent{}, Session{ID: "x", Store: kiroStoreV2}, false},
		"Kiro v3":     {KiroAgent{}, Session{ID: "x", Store: kiroStoreV3}, false},
		"Claude Code": {ClaudeCodeAgent{}, Session{ID: "x"}, false},
		"Codex":       {CodexAgent{}, Session{ID: "x"}, true},
		"Hermes":      {HermesAgent{}, Session{ID: "x"}, true},
	} {
		refusal := tc.a.RelocateRefusal(tc.s)
		if (refusal != nil) != tc.refuse {
			t.Errorf("%s: RelocateRefusal = %v, want refuse=%v", name, refusal, tc.refuse)
			continue
		}
		if !tc.refuse {
			continue
		}
		if !errors.Is(refusal, ErrRelocateUnsupported) {
			t.Errorf("%s: refusal %v does not wrap ErrRelocateUnsupported", name, refusal)
		}
		if _, err := tc.a.Relocate(tc.s, "/elsewhere", false); err == nil || err.Error() != refusal.Error() {
			t.Errorf("%s: Relocate = %v, want the refusal %v", name, err, refusal)
		}
	}
}
