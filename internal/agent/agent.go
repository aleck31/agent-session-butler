// Package agent models AI coding agents whose chat sessions live locally —
// as files (Kiro, Claude Code) or in a local SQLite DB (Hermes). Add support
// for a new agent by implementing Agent and registering it in the store's
// registry. Agents are peers — registration order is cosmetic.
package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// Store names the backing store when an agent keeps more than one and the same
	// id can appear in each. Kiro does: "v1" and "v2", the names its own CLI uses.
	// Empty for agents with a single store.
	Store string `json:"store"`

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
	// Relocate re-homes the session to newCwd. When asCopy is false (move) the session's cwd association changes in place;
	// when true (copy) the original is kept and a fresh copy is created under a new id. Returns the resulting session's id.
	// Agents that can't relocate (Hermes, Codex) return ErrRelocateUnsupported.
	Relocate(s Session, newCwd string, asCopy bool) (newID string, err error)
	// Rename sets the session's title in the agent's own metadata, so the new
	// title is what that agent shows too. Each backend stores a title somewhere
	// different and every one of them has an owner-blessed way to change it, so
	// none of these write around the agent. Refusing a locked session is the
	// store's job. Agents that can't rename return ErrRenameUnsupported.
	Rename(s Session, title string) error
}

// ErrRelocateUnsupported is returned by agents that don't support relocating.
var ErrRelocateUnsupported = errors.New("this agent does not support moving/copying sessions")

// ErrRenameUnsupported is returned by agents that don't support renaming.
var ErrRenameUnsupported = errors.New("this agent does not support renaming sessions")

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

// copyPath copies a file (or directory tree) from src to dst — used when
// copying a session's file bundle.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyTree(src, dst, info)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

func copyTree(src, dst string, info os.FileInfo) error {
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// clampTitle collapses a title to one line and clamps it for a table column.
// Agents whose "title" is really a raw first prompt (Kiro, Codex) need this —
// those run to tens of KB.
func clampTitle(s string) string {
	oneLine := strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r'
	}), " ")
	oneLine = strings.TrimSpace(oneLine)
	if len([]rune(oneLine)) > 80 {
		return string([]rune(oneLine)[:80]) + "…"
	}
	return oneLine
}

// shortTail returns a session id's last 8 characters, for a display placeholder.
// The tail, not the head: Hermes and Codex ids are time-ordered, so leading
// characters are shared between sessions and the tail is what distinguishes
// them. Ids are opaque and must never be parsed for meaning.
func shortTail(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
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

// appendBlock joins consecutive text blocks with a blank line, so several parts
// of one message read as paragraphs rather than running together.
func appendBlock(existing, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	return existing + "\n\n" + add
}
