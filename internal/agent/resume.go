package agent

// Resumer is implemented by agents whose CLI can reopen a session. shared reports that the id is
// also in another of this agent's stores. The binary stays bare: the user runs the command in
// their own shell, from the session's cwd, possibly on another host — not with asbutler's PATH.
type Resumer interface {
	ResumeArgv(s Session, shared bool) []string
}

func (ClaudeCodeAgent) ResumeArgv(s Session, _ bool) []string {
	return []string{"claude", "--resume", s.ID}
}

func (CodexAgent) ResumeArgv(s Session, _ bool) []string {
	return []string{"codex", "resume", s.ID}
}

// ResumeArgv reaches a v1 row only with --agent-engine v1 when v2 holds the same id. Never pass
// it for v2 — explicit v2 takes a path that demands input without a TTY — and --session-source
// is not a selector here: kiro-cli accepts it only with --delete-session.
func (KiroAgent) ResumeArgv(s Session, shared bool) []string {
	argv := []string{"kiro-cli", "chat", "--resume-id", s.ID}
	if shared && s.Store == kiroStoreV1 {
		argv = append(argv, "--agent-engine", "v1")
	}
	return argv
}
