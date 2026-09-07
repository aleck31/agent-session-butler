package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// KiroAgent: Kiro CLI stores each session as a bundle under
// ~/.kiro/sessions/cli/:
//
//	{sid}.json    — metadata (cwd, title, created_at, updated_at)
//	{sid}.jsonl   — conversation event stream (the bulk of the size)
//	{sid}.lock    — process lock, present while a process holds the session
//	{sid}.history — command history (interactive chat only)
//	{sid}/        — occasional per-session subdirectory
//
// The .json `cwd` field is the sole cwd↔session mapping; there is no index.
type KiroAgent struct{}

func (KiroAgent) Name() string { return "Kiro" }

func kiroSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kiro", "sessions", "cli")
}

func (KiroAgent) Installed() bool {
	dir := kiroSessionsDir()
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func (a KiroAgent) Scan() []Session {
	dir := kiroSessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	// Group every entry by its session id (the file-name stem).
	bundles := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		sid := strings.TrimSuffix(name, filepath.Ext(name))
		bundles[sid] = append(bundles[sid], filepath.Join(dir, name))
	}

	var out []Session
	for sid, paths := range bundles {
		if s, ok := a.parse(sid, paths); ok {
			out = append(out, s)
		}
	}
	return out
}

func (a KiroAgent) parse(sid string, paths []string) (Session, bool) {
	var jsonPath string
	for _, p := range paths {
		if filepath.Ext(p) == ".json" {
			jsonPath = p
			break
		}
	}
	if jsonPath == "" {
		return Session{}, false // no metadata → not a real session bundle
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return Session{}, false
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return Session{}, false
	}

	cwd := "(unknown)"
	if c, ok := meta["cwd"].(string); ok && c != "" {
		cwd = c
	}
	title := cleanTitle(strOrNil(meta["title"]), sid)

	// Sum the size of every file in the bundle; track newest mtime.
	var totalSize int64
	newest := time.Time{}
	var lockPath string
	for _, p := range paths {
		if filepath.Ext(p) == ".lock" {
			lockPath = p
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		totalSize += info.Size()
		if m := info.ModTime(); m.After(newest) {
			newest = m
		}
	}

	locked := lockPath != "" && kiroLockLive(lockPath)

	return Session{
		ID:           sid,
		Agent:        a.Name(),
		Cwd:          cwd,
		Title:        title,
		MessageCount: nil, // counting jsonl lines is slow; done lazily on demand
		FileSize:     totalSize,
		ModifiedAt:   newest,
		Locked:       locked,
		CacheKey:     jsonPath,
		FilePaths:    paths,
	}, true
}

// Delete removes the session's file bundle. Kiro sessions are plain files with
// no shadow index, so removing them directly is safe.
func (KiroAgent) Delete(s Session) error { return deleteFiles(s) }

// Relocate re-homes a Kiro session to newCwd. Move rewrites the .json's cwd in
// place; copy duplicates the whole bundle under a fresh session id first.
func (KiroAgent) Relocate(s Session, newCwd string, asCopy bool) (string, error) {
	jsonPath := s.CacheKey // Kiro's CacheKey is the .json path
	sid := s.ID
	if asCopy {
		newSid := uuid.NewString()
		for _, src := range s.FilePaths {
			// {oldsid}.ext → {newsid}.ext, preserving each file's extension/suffix.
			dst := filepath.Join(filepath.Dir(src), newSid+strings.TrimPrefix(filepath.Base(src), sid))
			if err := copyPath(src, dst); err != nil {
				return "", err
			}
			if filepath.Ext(src) == ".json" {
				jsonPath = dst
			}
		}
		sid = newSid
	}
	if err := kiroSetJSON(jsonPath, sid, newCwd); err != nil {
		return "", err
	}
	return sid, nil
}

// kiroSetJSON rewrites the session's .json with the given session_id and cwd,
// preserving all other fields.
func kiroSetJSON(jsonPath, sid, cwd string) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	m["session_id"] = sid
	m["cwd"] = cwd
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, out, 0o644)
}

// Enrich fills only the message count for Kiro (the title is already known from
// .json at scan time), by streaming the .jsonl event log and tallying Prompt
// (user) and AssistantMessage events. Tool results and compaction events are
// excluded — they aren't conversation messages.
func (a KiroAgent) Enrich(s Session) Session {
	var jsonlPath string
	for _, p := range s.FilePaths {
		if filepath.Ext(p) == ".jsonl" {
			jsonlPath = p
			break
		}
	}
	count := 0
	if jsonlPath != "" {
		forEachLine(jsonlPath, func(line string) bool {
			var obj map[string]any
			if json.Unmarshal([]byte(line), &obj) != nil {
				return true
			}
			switch obj["kind"] {
			case "Prompt", "AssistantMessage":
				count++
			}
			return true
		})
	}
	s.MessageCount = &count
	return s
}

// kiroLockLive is true only if the lock exists AND its PID is a live process.
func kiroLockLive(lockPath string) bool {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return false
	}
	pid, ok := obj["pid"].(float64) // JSON numbers decode to float64
	if !ok {
		return false
	}
	return pidAlive(int(pid))
}

// cleanTitle recovers readable text from Kiro's occasionally-JSON title blobs,
// strips noise wrappers, and falls back to a friendly placeholder so the list
// never shows JSON soup.
func cleanTitle(raw *string, sid string) string {
	shortID := sid
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	placeholder := "(untitled · " + shortID + ")"

	if raw == nil {
		return placeholder
	}
	t := strings.TrimSpace(*raw)
	if t == "" {
		return placeholder
	}

	// If it parses as the content-array shape, pull the first text part.
	if strings.HasPrefix(t, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(t), &obj) == nil {
			if parts, ok := obj["content"].([]any); ok {
				for _, p := range parts {
					m, ok := p.(map[string]any)
					if ok && m["type"] == "text" {
						if txt, ok := m["text"].(string); ok {
							t = strings.TrimSpace(txt)
							break
						}
					}
				}
			}
		}
	}

	// Drop a leading <untrusted_content_…> guard marker if present.
	if strings.HasPrefix(t, "<untrusted_content_") {
		if i := strings.IndexByte(t, '>'); i >= 0 {
			t = strings.TrimSpace(t[i+1:])
		}
	}

	// Still JSON-looking or empty → unusable; show a stable placeholder.
	if t == "" || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		return placeholder
	}

	return clampTitle(t)
}

func strOrNil(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}
