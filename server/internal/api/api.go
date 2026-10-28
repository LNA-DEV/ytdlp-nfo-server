// Package api implements the HTTP API (/api/v1, legacy /api routes), the
// global event stream and serves the embedded web UI (§9).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

type settingsT = config.Settings

// Server is the HTTP layer.
type Server struct {
	m          *manager.Manager
	static     fs.FS
	limiter    *limiter
	legacyOnce sync.Once
	mux        *http.ServeMux
}

// New builds the router. static is the web UI file system.
func New(m *manager.Manager, static fs.FS) *Server {
	s := &Server{m: m, static: static, limiter: &limiter{fails: map[string]*failState{}}, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP adds security headers and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	mux := s.mux
	a := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAuth(h, false) }

	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// v1
	mux.HandleFunc("GET /api/v1/auth", s.handleAuthStatus)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.handleLogout)
	mux.HandleFunc("POST /api/v1/jobs", a(s.handleCreateJob))
	mux.HandleFunc("POST /api/v1/jobs/bulk", a(s.handleBulk))
	mux.HandleFunc("GET /api/v1/jobs", a(s.handleListJobs))
	mux.HandleFunc("POST /api/v1/jobs/clear", a(s.handleClear))
	mux.HandleFunc("GET /api/v1/jobs/{id}", a(s.handleGetJob))
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", a(s.handleDeleteJob))
	mux.HandleFunc("GET /api/v1/jobs/{id}/log", a(s.handleLog))
	mux.HandleFunc("GET /api/v1/jobs/{id}/log/stream", a(s.handleLogStream))
	mux.HandleFunc("POST /api/v1/jobs/{id}/retry", a(s.handleRetry))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", a(s.handleCancel))
	mux.HandleFunc("POST /api/v1/jobs/{id}/start", a(s.handleStartNow))
	mux.HandleFunc("POST /api/v1/jobs/{id}/items/{item}/extras", a(s.handleExtras))
	mux.HandleFunc("PATCH /api/v1/queue", a(s.handleReorder))
	mux.HandleFunc("GET /api/v1/events", a(s.handleEvents))
	mux.HandleFunc("GET /api/v1/settings", a(s.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", a(s.handlePutSettings))
	mux.HandleFunc("PUT /api/v1/settings/password", a(s.handlePassword))
	mux.HandleFunc("GET /api/v1/options", a(s.handleOptions))
	mux.HandleFunc("GET /api/v1/ytdlp/channels/{ch}/releases", a(s.handleReleases))
	mux.HandleFunc("GET /api/v1/ytdlp/versions", a(s.handleVersions))
	mux.HandleFunc("POST /api/v1/ytdlp/versions", a(s.handleInstall))
	mux.HandleFunc("POST /api/v1/ytdlp/versions/{ch}/{tag}/activate", a(s.handleActivate))
	mux.HandleFunc("POST /api/v1/ytdlp/versions/{ch}/{tag}/pin", a(s.handlePin))
	mux.HandleFunc("POST /api/v1/ytdlp/versions/{ch}/{tag}/test", a(s.handleRetest))
	mux.HandleFunc("DELETE /api/v1/ytdlp/versions/{ch}/{tag}", a(s.handleDeleteVersion))
	mux.HandleFunc("POST /api/v1/ytdlp/check", a(s.handleCheckUpdates))
	mux.HandleFunc("GET /api/v1/sites", a(s.handleListSites))
	mux.HandleFunc("POST /api/v1/sites", a(s.handleSaveSite))
	mux.HandleFunc("PUT /api/v1/sites/{id}", a(s.handleSaveSite))
	mux.HandleFunc("DELETE /api/v1/sites/{id}", a(s.handleDeleteSite))
	mux.HandleFunc("GET /api/v1/cookies", a(s.handleListCookies))
	mux.HandleFunc("POST /api/v1/cookies", a(s.handleUploadCookies))
	mux.HandleFunc("DELETE /api/v1/cookies/{name}", a(s.handleDeleteCookies))
	mux.HandleFunc("GET /api/v1/tokens", a(s.handleListTokens))
	mux.HandleFunc("POST /api/v1/tokens", a(s.handleCreateToken))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", a(s.handleDeleteToken))
	mux.HandleFunc("GET /api/v1/archive", a(s.handleArchive))
	mux.HandleFunc("DELETE /api/v1/archive/{key}", a(s.handleArchiveDelete))
	mux.HandleFunc("POST /api/v1/archive/import", a(s.handleArchiveImport))
	mux.HandleFunc("GET /api/v1/archive/export", a(s.handleArchiveExport))
	mux.HandleFunc("POST /api/v1/archive/check", a(s.handleArchiveCheck))
	mux.HandleFunc("POST /api/v1/archive/fix", a(s.handleArchiveFix))
	mux.HandleFunc("GET /api/v1/system", a(s.handleSystem))
	mux.HandleFunc("POST /api/v1/system/test-url", a(s.handleTestURL))
	mux.HandleFunc("POST /api/v1/system/resume", a(s.handleResume))
	mux.HandleFunc("POST /api/v1/reddit/reset", a(s.handleRedditReset))
	mux.HandleFunc("DELETE /api/v1/system/warnings/{key}", a(s.handleClearWarning))
	mux.HandleFunc("POST /api/v1/subscriptions/preview", a(s.handleSubPreview))
	mux.HandleFunc("GET /api/v1/subscriptions", a(s.handleListSubs))
	mux.HandleFunc("POST /api/v1/subscriptions", a(s.handleCreateSub))
	mux.HandleFunc("POST /api/v1/subscriptions/check", a(s.handleCheckAll))
	mux.HandleFunc("POST /api/v1/subscriptions/entries/download", a(s.handleDownloadEntries))
	mux.HandleFunc("POST /api/v1/subscriptions/entries/ignore", a(s.handleIgnoreEntries))
	mux.HandleFunc("POST /api/v1/subscriptions/import", a(s.handleImportSubs))
	mux.HandleFunc("GET /api/v1/subscriptions/export", a(s.handleExportSubs))
	mux.HandleFunc("GET /api/v1/subscriptions/{id}", a(s.handleGetSub))
	mux.HandleFunc("PATCH /api/v1/subscriptions/{id}", a(s.handlePatchSub))
	mux.HandleFunc("DELETE /api/v1/subscriptions/{id}", a(s.handleDeleteSub))
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/check", a(s.handleCheckSub))
	mux.HandleFunc("GET /api/v1/subscriptions/{id}/log", a(s.handleSubLog))
	mux.HandleFunc("GET /api/v1/subscriptions/{id}/entries", a(s.handleSubEntries))
	mux.HandleFunc("GET /api/v1/feed", a(s.handleFeed))
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "not found") })

	// Legacy routes (v1 server, browser add-on): kept as aliases.
	mux.HandleFunc("POST /api/download", a(s.legacySubmit))
	mux.HandleFunc("POST /api/download/bulk", a(s.legacyBulk))
	mux.HandleFunc("GET /api/jobs", a(s.legacyList))
	mux.HandleFunc("DELETE /api/jobs", a(s.legacyDeleteAll))
	mux.HandleFunc("GET /api/jobs/{id}", a(s.legacyGet))
	mux.HandleFunc("GET /api/jobs/{id}/stream", s.requireAuth(s.legacyStream, true))
	mux.HandleFunc("POST /api/jobs/{id}/retry", a(s.legacyRetry))
	mux.HandleFunc("DELETE /api/jobs/{id}", a(s.legacyDelete))
	mux.HandleFunc("GET /api/auth", a(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
	}))
	mux.HandleFunc("GET /api/version", a(s.legacyVersion))

	if s.static != nil {
		files := http.FileServer(http.FS(s.static))
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: http: data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
			files.ServeHTTP(w, r)
		}))
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErr maps manager errors to HTTP statuses.
func writeErr(w http.ResponseWriter, err error) {
	var dup *manager.DuplicateError
	switch {
	case errors.As(err, &dup):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, manager.ErrInvalid):
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), manager.ErrInvalid.Error()+": "))
	case errors.Is(err, manager.ErrConflict):
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), manager.ErrConflict.Error()+": "))
	case errors.Is(err, manager.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.m.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}
