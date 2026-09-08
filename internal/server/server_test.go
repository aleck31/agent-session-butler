package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// sandboxServer points every agent's discovery at empty temp dirs, so handler
// tests never see the developer's real sessions, then returns a ready handler.
func sandboxServer(t *testing.T) http.Handler {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))
	return New("test-version").Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// /api/groups is the web UI's entry point: it must serve the nested view
// unenriched (ADR-0002 D2 — the UI enriches one group at a time on demand).
func TestGroupsServesTheNestedViewWithVersion(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodGet, "/api/groups", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	// The grouped shape is flat summary fields + a groups array (not {summary,…}).
	if _, ok := doc["groups"]; !ok {
		t.Error(`missing "groups" array`)
	}
	if doc["version"] != "test-version" {
		t.Errorf("version: got %v, want the version passed to New", doc["version"])
	}
	if doc["agentUsage"] == nil {
		t.Error("agentUsage: got null, want []")
	}
}

// The routes are method-scoped; a wrong method must not fall through to the
// static file server and return the UI's index.html with a 200.
func TestAPIRoutesRejectTheWrongMethod(t *testing.T) {
	h := sandboxServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/groups"},
		{http.MethodGet, "/api/enrich"},
		{http.MethodGet, "/api/session/abc"},
		{http.MethodPost, "/api/session/abc"},
		{http.MethodGet, "/api/session/abc/relocate"},
		{http.MethodGet, "/api/session/abc/rename"},
	} {
		w := do(t, h, tc.method, tc.path, `{}`)
		if w.Code == http.StatusOK {
			t.Errorf("%s %s: got 200, want a rejection", tc.method, tc.path)
		}
	}
}

func TestEnrichRequiresACwd(t *testing.T) {
	h := sandboxServer(t)
	for name, body := range map[string]string{
		"empty body":    "",
		"not json":      "garbage",
		"missing cwd":   `{"profile":"default"}`,
		"empty cwd":     `{"cwd":""}`,
		"wrong type":    `{"cwd":123}`,
		"json but null": `null`,
	} {
		t.Run(name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/api/enrich", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400", w.Code)
			}
			assertErrorBody(t, w)
		})
	}
}

func TestEnrichUnknownGroupIs404(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodPost, "/api/enrich", `{"cwd":"/definitely/not/here"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
	assertErrorBody(t, w)
}

// A group is identified by cwd AND profile — the same cwd under a different
// profile is a different group (ADR-0001 D5), so a profile mismatch is a 404.
func TestEnrichMatchesOnProfileToo(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodPost, "/api/enrich", `{"cwd":"/x","profile":"nonexistent-profile"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
}

// Deleting an id that doesn't exist is a 409 with a readable error, not a 500.
func TestDeleteUnknownSessionIsAConflictWithAMessage(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodDelete, "/api/session/no-such-id", "")
	if w.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", w.Code)
	}
	msg := assertErrorBody(t, w)
	if !strings.Contains(msg, "no-such-id") {
		t.Errorf("error message %q should name the id", msg)
	}
}

func TestRelocateRequiresANewCwd(t *testing.T) {
	h := sandboxServer(t)
	for name, body := range map[string]string{
		"empty body":     "",
		"not json":       "garbage",
		"missing newCwd": `{"copy":true}`,
		"empty newCwd":   `{"newCwd":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/api/session/abc/relocate", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400", w.Code)
			}
			assertErrorBody(t, w)
		})
	}
}

func TestRelocateUnknownSessionIsAConflict(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodPost, "/api/session/no-such-id/relocate", `{"newCwd":"/new"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", w.Code)
	}
	assertErrorBody(t, w)
}

// ADR-0002 D4: the UI is embedded, so the binary needs no network and ships as a
// single file. If go:embed ever stops picking web/ up, this is what catches it.
func TestEmbeddedUIIsServedFromTheBinary(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("GET / did not serve an HTML document")
	}
	// Alpine is vendored, not pulled from a CDN.
	if strings.Contains(body, "//unpkg.com") || strings.Contains(body, "//cdn.") {
		t.Error("the UI references an external CDN — it must be fully self-contained")
	}

	w = do(t, h, http.MethodGet, "/alpine.min.js", "")
	if w.Code != http.StatusOK {
		t.Errorf("GET /alpine.min.js: got %d, want 200 (vendored asset missing?)", w.Code)
	}
}

// Every file under web/ must be reachable, so a newly added asset can't be
// silently missing from the embed.
func TestEveryEmbeddedAssetIsReachable(t *testing.T) {
	h := sandboxServer(t)
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("web/ embedded nothing")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// FileServer redirects /index.html to / — either way the asset is there.
		w := do(t, h, http.MethodGet, "/"+e.Name(), "")
		if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently {
			t.Errorf("GET /%s: got %d, want 200 (or a redirect)", e.Name(), w.Code)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h := sandboxServer(t)
	if w := do(t, h, http.MethodGet, "/no/such/asset.js", ""); w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
}

// assertErrorBody checks the response is a JSON {"error": "..."} and returns it,
// so the UI can always surface a message rather than a blank failure.
func assertErrorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var doc map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, w.Body.String())
	}
	if doc["error"] == "" {
		t.Errorf(`expected a non-empty "error" field, got %s`, w.Body.String())
	}
	return doc["error"]
}

// Sanity check on the sandbox itself: if HOME weren't redirected these tests
// would be scanning the developer's real session history.
func TestSandboxIsolatesDiscovery(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodGet, "/api/groups", "")
	var doc struct {
		Agents      []string `json:"agents"`
		TotalGroups int      `json:"totalGroups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.TotalGroups != 0 {
		t.Errorf("sandbox found %d groups — HOME is not isolated (HOME=%s)", doc.TotalGroups, os.Getenv("HOME"))
	}
	if len(doc.Agents) != 0 {
		t.Errorf("sandbox discovered agents %v — discovery is not isolated", doc.Agents)
	}
}

func TestRenameRequiresATitle(t *testing.T) {
	h := sandboxServer(t)
	for name, body := range map[string]string{
		"empty body":    "",
		"not json":      "garbage",
		"missing title": `{"other":1}`,
		"empty title":   `{"title":""}`,
		"blank title":   `{"title":"   "}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/api/session/abc/rename", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400", w.Code)
			}
			assertErrorBody(t, w)
		})
	}
}

func TestRenameUnknownSessionIsAConflict(t *testing.T) {
	h := sandboxServer(t)
	w := do(t, h, http.MethodPost, "/api/session/no-such-id/rename", `{"title":"x"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", w.Code)
	}
	if msg := assertErrorBody(t, w); !strings.Contains(msg, "no-such-id") {
		t.Errorf("error %q should name the id", msg)
	}
}

func TestTranscriptEndpoint(t *testing.T) {
	h := sandboxServer(t)
	// Unknown session is a conflict with a readable message, not a 500.
	w := do(t, h, http.MethodGet, "/api/session/no-such-id/transcript", "")
	if w.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", w.Code)
	}
	if msg := assertErrorBody(t, w); !strings.Contains(msg, "no-such-id") {
		t.Errorf("error %q should name the id", msg)
	}
	// Query parameters are tolerated rather than rejected; a bad number just
	// leaves the default window in place.
	for _, q := range []string{"", "?tail=3", "?head=2", "?all=1", "?tools=1", "?tail=notanumber"} {
		w := do(t, h, http.MethodGet, "/api/session/no-such-id/transcript"+q, "")
		if w.Code != http.StatusConflict {
			t.Errorf("%q: got %d, want 409", q, w.Code)
		}
	}
	// GET only.
	if w := do(t, h, http.MethodPost, "/api/session/abc/transcript", "{}"); w.Code == http.StatusOK {
		t.Error("POST should not be accepted")
	}
}

// writeKiroV1 seeds a Kiro v1 store in a sandboxed HOME. macOS-only path, which
// is where the duplicate-id case was found and where these assertions run.
func writeKiroV1(t *testing.T, home string, rows [][2]string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("v1 store path is only wired for darwin here")
	}
	dir := filepath.Join(home, "Library", "Application Support", "kiro-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "data.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversations_v2 (key TEXT NOT NULL,
		conversation_id TEXT NOT NULL, value TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		PRIMARY KEY (key, conversation_id))`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		blob := `{"conversation_id":"` + r[1] + `","history":[{"user":{"content":{"Prompt":{"prompt":"q"}}},` +
			`"assistant":{"Response":{"content":"a"}}}]}`
		if _, err := db.Exec(`INSERT INTO conversations_v2 VALUES (?,?,?,?,?)`,
			r[0], r[1], blob, 1000, 2000); err != nil {
			t.Fatal(err)
		}
	}
}

// The HTTP layer has to pass the query's store and path through to the guard.
// Whether allWithID then gets past it is covered at the store level, with a fake
// agent — asserting it here would shell out to the real kiro-cli.
func TestDeleteRefusesASharedIDEvenWithACwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))
	writeKiroV1(t, home, [][2]string{{"/proj/a", "dupe"}, {"/proj/b", "dupe"}})
	h := New("test-version").Handler()

	// A cwd must not buy precision the agent cannot deliver, so this stays a refusal.
	w := do(t, h, http.MethodDelete, "/api/session/dupe?store=v1&path=/proj/a", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", w.Code)
	}
	if msg := assertErrorBody(t, w); !strings.Contains(msg, "--all-with-id") {
		t.Errorf("error %q should name the flag that proceeds", msg)
	}
}
