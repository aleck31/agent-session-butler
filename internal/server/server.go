// Package server exposes the session store over HTTP with an embedded browser
// UI. The core logic (scan/enrich/delete) lives in internal/store; this layer
// only adapts it to JSON + static assets, so the CLI and the UI share one core.
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/aleck/agent-session-butler/internal/store"
)

//go:embed web
var webFS embed.FS

// Server holds the store and routing.
type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

// New builds a server backed by the default store.
func New() *Server {
	s := &Server{store: store.New(), mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/groups", s.handleGroups)
	s.mux.HandleFunc("POST /api/enrich", s.handleEnrich)
	s.mux.HandleFunc("DELETE /api/session/{id}", s.handleDelete)

	// Static UI from the embedded web/ dir, served at the root.
	sub, _ := fs.Sub(webFS, "web")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
}

// Handler exposes the router (useful for tests and for wrapping with middleware).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts the HTTP server on addr (e.g. "127.0.0.1:7777").
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}

// --- view models -----------------------------------------------------------

// sessionView is one session as sent to the browser.
type sessionView struct {
	ID           string    `json:"id"`
	Agent        string    `json:"agent"`
	Title        string    `json:"title"`
	MessageCount *int      `json:"messageCount"`
	FileSize     int64     `json:"fileSize"`
	SizeHuman    string    `json:"sizeHuman"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	Locked       bool      `json:"locked"`
}

// groupView is one cwd group, with the derived flags the UI needs (Group's
// CwdExists/TotalSize/DisplayName are methods, not serialized fields).
type groupView struct {
	Cwd            string        `json:"cwd"`
	DisplayName    string        `json:"displayName"`
	Orphan         bool          `json:"orphan"` // working directory no longer exists
	SessionCount   int           `json:"sessionCount"`
	TotalSize      int64         `json:"totalSize"`
	TotalSizeHuman string        `json:"totalSizeHuman"`
	LatestModified time.Time     `json:"latestModified"`
	Sessions       []sessionView `json:"sessions"`
}

// summaryView is the top-of-page rollup.
type summaryView struct {
	Agents      []string    `json:"agents"`
	TotalGroups int         `json:"totalGroups"`
	TotalCount  int         `json:"totalCount"`
	TotalSize   int64       `json:"totalSize"`
	OrphanCount int         `json:"orphanCount"`
	OrphanSize  int64       `json:"orphanSize"`
	Groups      []groupView `json:"groups"`
}

func toSessionView(s store.Group) []sessionView {
	out := make([]sessionView, 0, len(s.Sessions))
	for _, sess := range s.Sessions {
		out = append(out, sessionView{
			ID:           sess.ID,
			Agent:        sess.Agent,
			Title:        sess.Title,
			MessageCount: sess.MessageCount,
			FileSize:     sess.FileSize,
			SizeHuman:    store.HumanSize(sess.FileSize),
			ModifiedAt:   sess.ModifiedAt,
			Locked:       sess.Locked,
		})
	}
	return out
}

func toGroupView(g store.Group) groupView {
	return groupView{
		Cwd:            g.Cwd,
		DisplayName:    g.DisplayName(),
		Orphan:         !g.CwdExists(),
		SessionCount:   len(g.Sessions),
		TotalSize:      g.TotalSize(),
		TotalSizeHuman: store.HumanSize(g.TotalSize()),
		LatestModified: g.LatestModified(),
		Sessions:       toSessionView(g),
	}
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups := s.store.Scan()

	sum := summaryView{Agents: s.store.InstalledAgents()}
	sum.Groups = make([]groupView, 0, len(groups))
	for _, g := range groups {
		gv := toGroupView(g)
		sum.Groups = append(sum.Groups, gv)
		sum.TotalGroups++
		sum.TotalCount += gv.SessionCount
		sum.TotalSize += gv.TotalSize
		if gv.Orphan {
			sum.OrphanCount++
			sum.OrphanSize += gv.TotalSize
		}
	}
	writeJSON(w, http.StatusOK, sum)
}

// handleEnrich enriches one cwd's sessions (message counts + titles) on demand,
// so the initial listing stays fast. Body: {"cwd": "..."}.
func (s *Server) handleEnrich(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cwd string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cwd == "" {
		writeError(w, http.StatusBadRequest, "expected JSON body with a non-empty \"cwd\"")
		return
	}
	for _, g := range s.store.Scan() {
		if g.Cwd == req.Cwd {
			writeJSON(w, http.StatusOK, toGroupView(s.store.EnrichGroup(g)))
			return
		}
	}
	writeError(w, http.StatusNotFound, "no directory group for that cwd")
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
