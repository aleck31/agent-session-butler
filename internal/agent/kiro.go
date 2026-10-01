package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if dir := kiroSessionsDir(); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	// A machine that only ever used the old store still has Kiro sessions.
	return fileExists(kiroDBPath())
}

// Scan covers both of Kiro's stores. The same id can appear in each — opening a
// v1 session copies it to v2 and leaves the original — so rows carry a Store and
// are not merged (ADR-0006).
func (a KiroAgent) Scan() []Session {
	return append(a.scanV2(), a.scanV1()...)
}

func (a KiroAgent) scanV2() []Session {
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

	meta, ok := kiroReadMeta(jsonPath)
	if !ok {
		return Session{}, false
	}

	cwd := "(unknown)"
	if meta.cwd != "" {
		cwd = meta.cwd
	}
	title := cleanTitle(meta.title, sid)

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
		Store:        kiroStoreV2,
		CacheKey:     jsonPath,
		FilePaths:    paths,
	}, true
}

// kiroMeta is the handful of fields Scan needs out of a session's .json.
type kiroMeta struct {
	cwd   string
	title *string // nil = absent or not a string
}

// kiroReadMeta streams a session's .json and stops as soon as it has cwd and
// title. This matters: the file also carries `session_state`, which holds the
// whole conversation — 274 KB on average and up to 2.7 MB here, 47.6 MB across
// all sessions — while cwd and title sit in the first few hundred bytes. Reading
// the file whole to pull two short strings was 95% of a `list` query's cost.
//
// Correctness does not depend on key order: if session_state came first we would
// decode and discard it, which is merely slow. Kiro writes it last today.
func kiroReadMeta(path string) (kiroMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return kiroMeta{}, false
	}
	defer f.Close()
	return kiroDecodeMeta(f)
}

// kiroDecodeMeta is kiroReadMeta over any reader, so a test can measure how much
// of the input is actually consumed — which is the property the speedup rests on,
// and the only way to assert it without timing something flaky.
func kiroDecodeMeta(r io.Reader) (kiroMeta, bool) {
	dec := json.NewDecoder(r)
	// Opening brace; anything else means this is not a session object.
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return kiroMeta{}, false
	}

	var meta kiroMeta
	sawCwd, sawTitle := false, false
	for dec.More() && !(sawCwd && sawTitle) {
		keyTok, err := dec.Token()
		if err != nil {
			return kiroMeta{}, false
		}
		key, _ := keyTok.(string)
		switch key {
		case "cwd":
			if err := dec.Decode(&meta.cwd); err != nil {
				return kiroMeta{}, false
			}
			sawCwd = true
		case "title":
			// Kiro sometimes stores a non-string here; treat that as absent and let
			// cleanTitle fall back to a placeholder.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return kiroMeta{}, false
			}
			var s string
			if json.Unmarshal(raw, &s) == nil {
				meta.title = &s
			}
			sawTitle = true
		default:
			// Consume and discard the value so the decoder stays aligned.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return kiroMeta{}, false
			}
		}
	}
	return meta, true
}

// Delete removes the session's file bundle. Kiro sessions are plain files with
// no shadow index, so removing them directly is safe.
func (a KiroAgent) Delete(s Session) error {
	if s.Store == kiroStoreV1 {
		return a.deleteV1(s)
	}
	return deleteFiles(s)
}

// Relocate re-homes a Kiro session to newCwd. Move rewrites the .json's cwd in
// place; copy duplicates the whole bundle under a fresh session id first.
func (KiroAgent) Relocate(s Session, newCwd string, asCopy bool) (string, error) {
	// v1 keys a conversation by cwd inside Kiro's own database; re-homing it would
	// mean writing that database, which is Kiro's to write.
	if s.Store == kiroStoreV1 {
		return "", fmt.Errorf("%w: not for Kiro's v1 store", ErrRelocateUnsupported)
	}
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
	return kiroPatchJSON(jsonPath, map[string]any{"session_id": sid, "cwd": cwd})
}

// Rename writes the new title into the session's .json, which is where Kiro
// reads it from — so Kiro's own listing shows it too.
func (KiroAgent) Rename(s Session, title string) error {
	// v1 has no title field: Kiro derives the label from the first user message, so
	// renaming would mean rewriting what the person typed. This tool changes a cwd
	// association and a title, never conversation content.
	if s.Store == kiroStoreV1 {
		return fmt.Errorf("%w: Kiro's v1 store has no title field — it shows the first message", ErrRenameUnsupported)
	}
	return kiroPatchJSON(s.CacheKey, map[string]any{"title": title}) // CacheKey is the .json path
}

// kiroPatchJSON sets the given fields in the session's .json, leaving every
// other field as it was. Reads the whole file rather than streaming: unlike
// Scan this has to write it all back anyway.
func kiroPatchJSON(jsonPath string, fields map[string]any) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for k, v := range fields {
		m[k] = v
	}
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
	if s.Store == kiroStoreV1 {
		return a.enrichV1(s)
	}
	var jsonlPath string
	for _, p := range s.FilePaths {
		if filepath.Ext(p) == ".jsonl" {
			jsonlPath = p
			break
		}
	}
	count := 0
	if jsonlPath != "" {
		forEachLineBytes(jsonlPath, func(line []byte) bool {
			if !completeLine(line) {
				return true
			}
			switch kind, _ := topLevelString(line, "kind"); kind {
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

// Transcript reads Kiro's .jsonl event stream. Kiro records no per-message
// timestamp, so Turn.At is always nil; a Prompt event is always real user input.
func (a KiroAgent) Transcript(s Session, opts TranscriptOptions) ([]Turn, error) {
	if s.Store == kiroStoreV1 {
		return a.transcriptV1(s, opts)
	}
	var jsonl string
	for _, p := range s.FilePaths {
		if filepath.Ext(p) == ".jsonl" {
			jsonl = p
			break
		}
	}
	if jsonl == "" {
		return nil, errors.New("session has no conversation file")
	}

	pending := map[string]*ToolCall{} // toolUse and its toolResult arrive separately
	var turns []Message
	var userAt []bool

	forEachLine(jsonl, func(line string) bool {
		var ev struct {
			Kind string `json:"kind"`
			Data struct {
				Content []struct {
					Kind string          `json:"kind"`
					Data json.RawMessage `json:"data"`
				} `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			return true
		}

		if ev.Kind == "ToolResults" {
			for _, part := range ev.Data.Content {
				if part.Kind != "toolResult" {
					continue
				}
				var tr struct {
					ToolUseID string `json:"toolUseId"`
					Content   []struct {
						Data string `json:"data"`
					} `json:"content"`
				}
				if json.Unmarshal(part.Data, &tr) != nil {
					continue
				}
				call, ok := pending[tr.ToolUseID]
				if !ok {
					continue
				}
				var body strings.Builder
				for _, c := range tr.Content {
					body.WriteString(c.Data)
				}
				call.OutputBytes = body.Len()
				if opts.Bodies {
					call.Output = readableBody(body.String())
				}
			}
			return true
		}

		role := ""
		switch ev.Kind {
		case "Prompt":
			role = "user"
		case "AssistantMessage":
			role = "assistant"
		default:
			return true
		}

		turn := Message{Role: role}
		for _, part := range ev.Data.Content {
			switch part.Kind {
			case "text":
				var t string
				if json.Unmarshal(part.Data, &t) == nil {
					turn.Text = appendBlock(turn.Text, t)
				}
			case "thinking":
				var t string
				if json.Unmarshal(part.Data, &t) == nil {
					turn.setThinking(t, opts.Bodies)
				}
			case "toolUse":
				var tu struct {
					ToolUseID string          `json:"toolUseId"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
				}
				if json.Unmarshal(part.Data, &tu) != nil {
					continue
				}
				args := string(tu.Input)
				call := ToolCall{Name: tu.Name, Summary: summariseArgs(args, toolSummaryRunes), ArgsBytes: len(args)}
				if opts.Bodies {
					call.Args = readableBody(args)
				}
				turn.Tools = append(turn.Tools, call)
				pending[tu.ToolUseID] = &turn.Tools[len(turn.Tools)-1]
			}
		}
		turns = append(turns, turn)
		userAt = append(userAt, role == "user")
		return true
	})

	// Grouping and windowing happen last: a tool result can arrive several events
	// after its call, so pending has to survive the whole stream.
	ex := groupTurns(turns, func(i int) bool { return userAt[i] })
	return windowTurns(ex, opts), nil
}
