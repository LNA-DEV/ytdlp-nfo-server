package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// --- settings ---

type settingsResponse struct {
	Settings        config.Settings `json:"settings"`
	PasswordSet     bool            `json:"passwordSet"`
	PasswordFromEnv bool            `json:"passwordFromEnv"`
	DefaultLibrary  string          `json:"defaultLibraryPath"`
	Channels        []string        `json:"channels"`
	// UsedBy lists the subscriptions whose options use a library, preset
	// or cookie profile, shown before it is removed (§5.2).
	UsedBy map[string]map[string][]string `json:"usedBySubscriptions"`
}

func (s *Server) settingsResponse() settingsResponse {
	st := s.m.Settings.Settings()
	resp := settingsResponse{
		PasswordSet: st.PasswordHash != "" || s.m.Env.Password != "", PasswordFromEnv: s.m.Env.Password != "",
		DefaultLibrary: s.m.Env.LibraryRoot(), Channels: config.Channels,
	}
	resp.UsedBy, _ = s.m.SubscriptionsUsing(context.Background())
	st.PasswordHash = ""
	st.Jellyfin.APIKey = mask(st.Jellyfin.APIKey)
	st.Notifications.GotifyToken = mask(st.Notifications.GotifyToken)
	resp.Settings = st
	return resp
}

const masked = "••••••••"

func mask(s string) string {
	if s == "" {
		return ""
	}
	return masked
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsResponse())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in config.Settings
	if !decode(w, r, &in, 256<<10) {
		return
	}
	if in.Naming.Preset == config.NamingCustom {
		if _, err := library.ParseTemplate(in.Naming.Template); err != nil {
			writeError(w, http.StatusBadRequest, "naming template: "+err.Error())
			return
		}
	}
	_, err := s.m.Settings.Update(r.Context(), func(st *config.Settings) error {
		keep := *st
		*st = in
		// Not editable here: password (own endpoint), active version
		// (activate endpoint), secrets sent back masked.
		st.PasswordHash = keep.PasswordHash
		st.Ytdlp.Active = keep.Ytdlp.Active
		if in.Jellyfin.APIKey == masked {
			st.Jellyfin.APIKey = keep.Jellyfin.APIKey
		}
		if in.Notifications.GotifyToken == masked {
			st.Notifications.GotifyToken = keep.Notifications.GotifyToken
		}
		if in.Subscriptions == (config.SubscriptionSettings{}) {
			st.Subscriptions = keep.Subscriptions // a client that predates subscriptions
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsResponse())
}

// handleOptions lists the choices for job options (UI and add-on).
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	st := s.m.Settings.Settings()
	presets := make([]string, 0, len(st.Presets))
	for k := range st.Presets {
		presets = append(presets, k)
	}
	sort.Slice(presets, func(i, j int) bool {
		order := map[string]int{"best": 0, "compatible": 1, "max1080": 2, "max720": 3}
		oi, iok := order[presets[i]]
		oj, jok := order[presets[j]]
		if iok && jok {
			return oi < oj
		}
		if iok != jok {
			return iok
		}
		return presets[i] < presets[j]
	})
	libs := []string{config.DefaultLibrary}
	for _, l := range st.Libraries {
		libs = append(libs, l.Name)
	}
	cookies, _ := s.m.Store.ListCookieProfiles(r.Context())
	cookieNames := []string{}
	for _, c := range cookies {
		cookieNames = append(cookieNames, c.Name)
	}
	targets := []string{}
	versions := []string{}
	if all, err := s.m.Store.ListVersions(r.Context()); err == nil {
		for _, v := range all {
			if v.SmokeOK {
				versions = append(versions, v.ID)
			}
			if v.ID == st.Ytdlp.Active {
				targets = append(targets, v.ImpersonateTargets...)
			}
		}
	}
	paths := map[string]string{}
	for _, l := range libs {
		paths[l], _ = st.LibraryPath(s.m.Env, l)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"presets": presets, "containers": []string{"mkv", "mp4"}, "libraries": libs, "cookieProfiles": cookieNames,
		"impersonateTargets": targets, "ytdlpVersions": versions, "defaults": st.Defaults,
		// For the path preview of the subscription dialog (§7.4).
		"libraryPaths": paths, "namingTemplate": st.Naming.EffectiveTemplate(),
		"subscriptionInterval": st.Subscriptions.IntervalMinutes,
		// The library marked for Reddit (Reddit concept §8): other ones get
		// the warning that Jellyfin's movie libraries don't show images.
		"redditLibrary": st.Reddit.DefaultLibrary,
	})
}

// --- yt-dlp versions ---

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("ch")
	if !config.ValidChannel(ch) {
		writeError(w, http.StatusNotFound, "unknown channel")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rels, err := s.m.Versions.Releases(ctx, ch)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	all, err := s.m.Store.ListVersions(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if all == nil {
		all = []*store.YtdlpVersion{}
	}
	st := s.m.Settings.Settings()
	writeJSON(w, http.StatusOK, map[string]any{
		"versions": all, "active": st.Ytdlp.Active, "channel": st.Ytdlp.Channel,
		"updates": s.m.Versions.Updates(), "managed": s.m.Env.YtdlpPath == "",
	})
}

func versionID(r *http.Request) string { return r.PathValue("ch") + "/" + r.PathValue("tag") }

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		Tag     string `json:"tag"`
	}
	if !decode(w, r, &req, 16<<10) {
		return
	}
	if !config.ValidChannel(req.Channel) || req.Tag == "" {
		writeError(w, http.StatusBadRequest, "channel and tag are required")
		return
	}
	// Installing continues even if the client goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	v, err := s.m.Versions.Install(ctx, req.Channel, req.Tag)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Versions.Activate(r.Context(), versionID(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, err)
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.handleVersions(w, r)
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if !decode(w, r, &req, 4<<10) {
		return
	}
	if err := s.m.Versions.SetPinned(r.Context(), versionID(r), req.Pinned); err != nil {
		writeErr(w, err)
		return
	}
	s.handleVersions(w, r)
}

func (s *Server) handleRetest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	v, err := s.m.Versions.Retest(ctx, versionID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Versions.Delete(r.Context(), versionID(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, err)
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.handleVersions(w, r)
}

func (s *Server) handleCheckUpdates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	s.m.Versions.CheckUpdates(ctx)
	s.handleVersions(w, r)
}

// --- sites ---

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.SiteRules())
}

func (s *Server) handleSaveSite(w http.ResponseWriter, r *http.Request) {
	var rule config.SiteRule
	if !decode(w, r, &rule, 64<<10) {
		return
	}
	if idStr := r.PathValue("id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		rule.ID = id
	} else {
		rule.ID = 0
	}
	saved, err := s.m.SaveSiteRule(r.Context(), rule)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.DeleteSiteRule(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- cookies ---

func (s *Server) handleListCookies(w http.ResponseWriter, r *http.Request) {
	list, err := s.m.Store.ListCookieProfiles(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleUploadCookies(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	var name string
	var data []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(5 << 20); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		name = r.FormValue("name")
		f, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "file is required")
			return
		}
		defer f.Close()
		data, _ = io.ReadAll(f)
	} else {
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		name, data = req.Name, []byte(req.Content)
	}
	p, err := s.m.SaveCookieProfile(r.Context(), name, data)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteCookies(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteCookieProfile(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- tokens ---

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.m.Store.ListTokens(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req, 4<<10) {
		return
	}
	plain, t, err := s.m.CreateToken(r.Context(), req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": plain, "info": t})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.Store.DeleteToken(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- archive ---

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, total, err := s.m.Store.SearchArchive(r.Context(), q.Get("q"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	if entries == nil {
		entries = []store.ArchiveEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
}

func (s *Server) handleArchiveDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.m.RemoveArchive(r.Context(), r.PathValue("key")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleArchiveImport(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.m.ImportArchive(r.Context(), data)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"imported": n})
}

func (s *Server) handleArchiveExport(w http.ResponseWriter, r *http.Request) {
	data, err := s.m.ExportArchive(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ytdlp-archive.txt"`)
	_, _ = w.Write(data)
}

// handleArchiveCheck compares the archive with the library (read-only).
func (s *Server) handleArchiveCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.m.CheckArchive(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleArchiveFix removes archive entries and optionally re-queues them.
func (s *Server) handleArchiveFix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys       []string `json:"keys"`
		Redownload bool     `json:"redownload"`
	}
	if !decode(w, r, &req, 4<<20) {
		return
	}
	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "keys are required")
		return
	}
	res, err := s.m.FixArchive(r.Context(), req.Keys, req.Redownload)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- system ---

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.System(r.Context()))
}

func (s *Server) handleTestURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL         string `json:"url"`
		Impersonate string `json:"impersonate"`
	}
	if !decode(w, r, &req, 16<<10) {
		return
	}
	out, err := s.m.TestURL(r.Context(), strings.TrimSpace(req.URL), req.Impersonate)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.m.Sched.Resume()
	s.m.ClearWarning("disk")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRedditReset starts a new Reddit session: no cookies, native no
// longer paused.
func (s *Server) handleRedditReset(w http.ResponseWriter, r *http.Request) {
	if err := s.m.ResetReddit(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleClearWarning(w http.ResponseWriter, r *http.Request) {
	s.m.ClearWarning(r.PathValue("key"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
