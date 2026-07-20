// Package agent models AI coding agents whose chat sessions live on disk.
// Add support for a new agent by implementing Agent and registering it in
// the store's registry. Agents are peers — registration order is cosmetic.
package agent

import (
	"bufio"
	"os"
	"time"
)

// Session is a single chat session (one bundle of files on disk).
type Session struct {
	ID           string    `json:"id"`           // sessionId (falls back to file-name stem)
	Agent        string    `json:"agent"`        // source agent, e.g. "Kiro" / "Claude Code"
	Cwd          string    `json:"cwd"`          // working directory this session belongs to
	Title        string    `json:"title"`        // display title (may be a placeholder until enriched)
	MessageCount *int      `json:"messageCount"` // nil = not counted yet (enriched lazily)
	FileSize     int64     `json:"fileSize"`     // total bytes of all files in this session
	ModifiedAt   time.Time `json:"modifiedAt"`   // newest mtime across the bundle
	Locked       bool      `json:"locked"`       // held by a live process (a running agent owns the lock)
	PrimaryPath  string    `json:"-"`            // primary file (.json / .jsonl), used as cache identity
	FilePaths    []string  `json:"-"`            // every file to remove when deleting this session
}

// Agent is an AI application whose sessions live on disk. Named Agent, not
// Provider, to avoid confusion with LLM/model providers.
type Agent interface {
	// Name is the display name, e.g. "Kiro".
	Name() string
	// Installed reports whether this agent is present on the machine (drives discovery).
	Installed() bool
	// Scan returns every session. Expensive fields (MessageCount, and for some
	// agents Title) may be left nil/placeholder here; Enrich fills them in.
	Scan() []Session
	// Enrich fills a session's lazily-computed fields in a single file read.
	// Potentially slow, so callers run it off the hot path and only when the
	// session is about to be shown. Returns an updated copy.
	Enrich(s Session) Session
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
