// Package server exposes the session store over HTTP with an embedded browser
// UI. The core logic (scan/enrich/delete) lives in internal/store; this layer
// only adapts it to JSON + static assets, so the CLI and the UI share one core.
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/aleck/agent-session-butler/internal/agent"
	"github.com/aleck/agent-session-butler/internal/store"
	"github.com/aleck/agent-session-butler/internal/view"
)

// Only the files the UI serves. A bare `embed web` would also ship md_test.mjs
// and the directory's README, and the static handler would serve them.
//
//go:embed web/index.html web/alpine.min.js web/md.js
var webFS embed.FS

// Server holds the store and routing.
type Server struct {
	store   *store.Store
	mux     *http.ServeMux
	version string
}

// New builds a server backed by the default store. version is surfaced to the
// UI (in the /api/groups summary) and is otherwise cosmetic.
func New(version string) *Server {
	s := &Server{store: store.New(), mux: http.NewServeMux(), version: version}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/groups", s.handleGroups)
	s.mux.HandleFunc("POST /api/enrich", s.handleEnrich)
	s.mux.HandleFunc("DELETE /api/session/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /api/session/{id}/relocate", s.handleRelocate)
	s.mux.HandleFunc("POST /api/session/{id}/rename", s.handleRename)
	s.mux.HandleFunc("GET /api/session/{id}/transcript", s.handleTranscript)

	// Static UI from the embedded web/ dir, served at the root.
	sub, _ := fs.Sub(webFS, "web")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
}

// Handler exposes the router; the cmd layer builds the http.Server and listener.
func (s *Server) Handler() http.Handler { return s.mux }

// --- handlers ---------------------------------------------------------------

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups := s.store.Scan()
	writeJSON(w, http.StatusOK, view.Grouped(s.store.InstalledAgents(), groups, s.version))
}

// handleEnrich enriches one group's sessions (message counts + titles) on
// demand, so the initial listing stays fast. A group is identified by cwd +
// profile (the same cwd under different Hermes profiles is distinct).
// Body: {"cwd": "...", "profile": "..."}.
func (s *Server) handleEnrich(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cwd     string `json:"cwd"`
		Profile string `json:"profile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cwd == "" {
		writeError(w, http.StatusBadRequest, "expected JSON body with a non-empty \"cwd\"")
		return
	}
	for _, g := range s.store.Scan() {
		if g.Cwd == req.Cwd && g.Profile == req.Profile {
			writeJSON(w, http.StatusOK, view.GroupView(s.store.EnrichGroup(g)))
			return
		}
	}
	writeError(w, http.StatusNotFound, "no directory group for that cwd/profile")
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	// store and path narrow an id that appears in more than one store, or more than
	// once within a store; allWithID confirms a delete the agent can only do as a set.
	q := r.URL.Query()
	removed, err := s.store.DeleteByID(id, q.Get("store"), q.Get("path"), q.Get("allWithID") != "")
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if len(removed) > 1 {
		// More went than was named, so say what — the caller's list is staler than it knows.
		dirs := make([]string, 0, len(removed))
		for _, sess := range removed {
			dirs = append(dirs, sess.Cwd)
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": len(removed), "cwds": dirs})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRelocate moves (or copies, if body.copy) a session to a new cwd.
// Body: {"newCwd": "...", "copy": bool}. Returns {"id": <resulting id>}.
func (s *Server) handleRelocate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		NewCwd string `json:"newCwd"`
		Copy   bool   `json:"copy"`
		Store  string `json:"store"`
		Path   string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewCwd == "" {
		writeError(w, http.StatusBadRequest, "expected JSON body with a non-empty \"newCwd\"")
		return
	}
	newID, resolved, err := s.store.RelocateByID(id, req.Store, req.Path, req.NewCwd, req.Copy)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// cwd is the normalised target, so the UI can show where it really went.
	writeJSON(w, http.StatusOK, map[string]string{"id": newID, "cwd": resolved})
}

// handleRename sets a session's title in the owning agent's own metadata.
// Body: {"title": "..."}. Returns {"title": <what was written>}, which is the
// trimmed form rather than the raw input.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Title string `json:"title"`
		Store string `json:"store"`
		Path  string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "expected JSON body with a non-empty \"title\"")
		return
	}
	title, err := s.store.RenameByID(id, req.Store, req.Path, req.Title)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"title": title})
}

// handleTranscript returns a session's conversation. Query: tail/head (turns,
// default agent.DefaultTurns), tools=1 for full bodies. Bounded by default
// because a whole session reaches 97 MB.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := agent.TranscriptOptions{Tail: agent.DefaultTurns, Bodies: q.Get("tools") != ""}
	if q.Get("all") != "" {
		opts.Tail = 0
	}
	if n, err := strconv.Atoi(q.Get("tail")); err == nil && n > 0 {
		opts.Tail, opts.Head = n, 0
	}
	if n, err := strconv.Atoi(q.Get("head")); err == nil && n > 0 {
		opts.Head, opts.Tail = n, 0
	}

	sess, turns, err := s.store.TranscriptByID(r.PathValue("id"), q.Get("store"), q.Get("path"), opts)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.ID, "agent": sess.Agent, "title": sess.Title,
		"cwd": sess.Cwd, "turns": turns,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
