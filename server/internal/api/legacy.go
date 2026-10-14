package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// Legacy (v1) request and response shapes. The browser add-on depends on
// POST /api/download returning 201 / 409.

type legacyReq struct {
	URL       string `json:"url"`
	Format    string `json:"format"`
	AllAudio  *bool  `json:"allAudio"`
	Subtitles *bool  `json:"subtitles"`
	// Newer add-ons may send v2 options too.
	Options *config.OptionsRequest `json:"options,omitempty"`
}

func (l legacyReq) request() config.OptionsRequest {
	var r config.OptionsRequest
	if l.Options != nil {
		r = *l.Options
	}
	if l.Format == "mp4" || l.Format == "mkv" {
		f := l.Format
		r.Container = &f
	}
	if l.AllAudio != nil {
		r.AllAudio = l.AllAudio
	}
	if l.Subtitles != nil {
		r.Subtitles = l.Subtitles
	}
	return r
}

type legacyOptions struct {
	Format    string `json:"format"`
	AllAudio  bool   `json:"allAudio"`
	Subtitles bool   `json:"subtitles"`
}

type legacySummary struct {
	ID         string        `json:"id"`
	URL        string        `json:"url"`
	Status     string        `json:"status"`
	CreatedAt  string        `json:"createdAt"`
	DoneAt     string        `json:"doneAt,omitempty"`
	Error      string        `json:"error,omitempty"`
	Progress   float64       `json:"progress"`
	RetryCount int           `json:"retryCount"`
	MaxRetries int           `json:"maxRetries"`
	Options    legacyOptions `json:"options"`
	Title      string        `json:"title,omitempty"`
}

type legacyDetail struct {
	legacySummary
	Output []string `json:"output"`
}

func legacyStatus(s store.JobState) string {
	switch s {
	case store.JobProbing, store.JobRunning:
		return "running"
	case store.JobPartial, store.JobCancelled:
		return "failed"
	}
	return string(s)
}

func legacyTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func (s *Server) toLegacy(v *manager.JobView) legacySummary {
	l := legacySummary{
		ID: v.ID, URL: v.URL, Status: legacyStatus(v.State), CreatedAt: legacyTime(v.CreatedAt), Error: v.Error,
		RetryCount: v.Attempts, MaxRetries: s.m.Settings.Settings().MaxRetries, Title: v.Title,
		Options: legacyOptions{Format: v.Options.Container, AllAudio: v.Options.AllAudio, Subtitles: v.Options.Subtitles},
	}
	if v.FinishedAt != nil {
		l.DoneAt = legacyTime(*v.FinishedAt)
	}
	switch {
	case v.State == store.JobCompleted:
		l.Progress = 100
	case v.Progress != nil:
		l.Progress = v.Progress.Percent
	}
	return l
}

func (s *Server) legacySubmit(w http.ResponseWriter, r *http.Request) {
	var req legacyReq
	if !decode(w, r, &req, 64<<10) {
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	j, err := s.m.Submit(r.Context(), req.URL, req.request())
	if err != nil {
		var dup *manager.DuplicateError
		if errors.As(err, &dup) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, err)
		return
	}
	v, err := s.m.Get(r.Context(), j.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.toLegacy(v))
}

func (s *Server) legacyBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs      []string `json:"urls"`
		Format    string   `json:"format"`
		AllAudio  *bool    `json:"allAudio"`
		Subtitles *bool    `json:"subtitles"`
	}
	if !decode(w, r, &req, 2<<20) {
		return
	}
	if len(req.URLs) > 500 {
		writeError(w, http.StatusBadRequest, "maximum 500 URLs allowed")
		return
	}
	opts := legacyReq{Format: req.Format, AllAudio: req.AllAudio, Subtitles: req.Subtitles}.request()
	res := s.m.SubmitBulk(r.Context(), req.URLs, opts)
	type item struct {
		URL    string         `json:"url"`
		Status string         `json:"status"`
		Job    *legacySummary `json:"job,omitempty"`
		Error  string         `json:"error,omitempty"`
	}
	out := struct {
		Created    int    `json:"created"`
		Duplicates int    `json:"duplicates"`
		Errors     int    `json:"errors"`
		Results    []item `json:"results"`
	}{Results: []item{}}
	for _, br := range res {
		it := item{URL: br.URL}
		switch {
		case br.Dup != nil:
			it.Status = "duplicate"
			out.Duplicates++
		case br.Err != nil:
			it.Status, it.Error = "error", strings.TrimPrefix(br.Err.Error(), manager.ErrInvalid.Error()+": ")
			out.Errors++
		default:
			it.Status = "created"
			if v, err := s.m.Get(r.Context(), br.Job.ID); err == nil {
				l := s.toLegacy(v)
				it.Job = &l
			}
			out.Created++
		}
		out.Results = append(out.Results, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) legacyList(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.m.List(r.Context(), manager.ListFilter{Limit: 1000})
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]legacySummary, 0, len(jobs))
	for i := range jobs {
		out = append(out, s.toLegacy(&jobs[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) legacyGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v, err := s.m.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out := legacyDetail{legacySummary: s.toLegacy(v), Output: s.m.Logs.Tail(id, 500)}
	if out.Output == nil {
		out.Output = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) legacyRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "job not found")
		return
	}
	if _, err := s.m.Retry(r.Context(), id, manager.RetryRequest{}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.m.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toLegacy(v))
}

func (s *Server) legacyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err := s.m.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) legacyDeleteAll(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteAll(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) legacyVersion(w http.ResponseWriter, r *http.Request) {
	v, _, _ := buildinfo.Info()
	out := map[string]string{"ytdlp-nfo-server": v}
	if act, err := s.m.Versions.Active(r.Context()); err == nil {
		out["yt-dlp"] = act.Version
	}
	writeJSON(w, http.StatusOK, out)
}
