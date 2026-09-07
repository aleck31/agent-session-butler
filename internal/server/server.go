// Package server exposes the session store over HTTP with an embedded browser
// UI. The core logic (scan/enrich/delete) lives in internal/store; this layer
// only adapts it to JSON + static assets, so the CLI and the UI share one core.
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/aleck/agent-session-butler/internal/store"
	"github.com/aleck/agent-session-butler/internal/view"
)

//go:embed web
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
	if err := s.store.DeleteByID(id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewCwd == "" {
		writeError(w, http.StatusBadRequest, "expected JSON body with a non-empty \"newCwd\"")
		return
	}
	newID, resolved, err := s.store.RelocateByID(id, req.NewCwd, req.Copy)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// cwd is the normalised target, so the UI can show where it really went.
	writeJSON(w, http.StatusOK, map[string]string{"id": newID, "cwd": resolved})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
