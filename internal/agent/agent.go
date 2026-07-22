// Package agent models AI coding agents whose chat sessions live locally —
// as files (Kiro, Claude Code) or in a local SQLite DB (Hermes). Add support
// for a new agent by implementing Agent and registering it in the store's
// registry. Agents are peers — registration order is cosmetic.
package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Session is a single chat session. For file-backed agents (Kiro, Claude Code)
// it's a bundle of files on disk; for DB-backed agents (Hermes) it's a row.
type Session struct {
	ID           string    `json:"id"`           // sessionId (falls back to file-name stem)
	Agent        string    `json:"agent"`        // source agent, e.g. "Kiro" / "Claude Code"
	Cwd          string    `json:"cwd"`          // working directory this session belongs to
	Title        string    `json:"title"`        // display title (may be a placeholder until enriched)
	MessageCount *int      `json:"messageCount"` // nil = not counted yet (enriched lazily)
	FileSize     int64     `json:"fileSize"`     // total bytes; 0 for DB-backed sessions
	ModifiedAt   time.Time `json:"modifiedAt"`   // newest mtime (file agents) / ended_at (Hermes)
	Locked       bool      `json:"locked"`       // held by a live process (a running agent owns the lock)

	// CacheKey uniquely identifies this session for the store's enrichment cache.
	// For file agents it's the primary file path; for Hermes it's db-path#id.
	CacheKey string `json:"-"`
	// FilePaths is every file to remove when deleting (file-backed agents only).
	FilePaths []string `json:"-"`
	// Extra carries agent-specific data the agent needs to act on this session
	// later (e.g. Hermes stores its profile's HERMES_HOME for the delete CLI).
	Extra map[string]string `json:"-"`
}

// Agent is an AI application whose sessions live on disk (files or a local DB).
// Named Agent, not Provider, to avoid confusion with LLM/model providers.
type Agent interface {
	// Name is the display name, e.g. "Kiro".
	Name() string
	// Installed reports whether this agent is present on the machine (drives discovery).
	Installed() bool
	// Scan returns every session. Expensive fields (MessageCount, and for some
	// agents Title) may be left nil/placeholder here; Enrich fills them in.
	Scan() []Session
	// Enrich fills a session's lazily-computed fields. Potentially slow, so
	// callers run it off the hot path and only when the session is about to be
	// shown. Returns an updated copy.
	Enrich(s Session) Session
	// Delete permanently removes the session. File agents remove their files;
	// Hermes shells out to its CLI (never touches the DB directly). Refusing a
	// locked session is the store's job, not the agent's.
	Delete(s Session) error
}

// deleteFiles removes every file in a file-backed session's bundle. Shared by
// the file agents (Kiro, Claude Code); Hermes overrides Delete entirely.
func deleteFiles(s Session) error {
	for _, p := range s.FilePaths {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("failed to delete %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// forEachLine streams a file line by line, calling handle for each until it
// returns false (stop) or EOF. Uses a large buffer because some Claude jsonl
// files have single lines of many MB.
func forEachLine(path string, handle func(line string) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Grow the token buffer to tolerate very long jsonl lines (up to 64 MB).
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		if !handle(sc.Text()) {
			return
		}
	}
}
