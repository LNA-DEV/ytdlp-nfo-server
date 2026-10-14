package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
)

type createReq struct {
	URL     string                `json:"url"`
	Options config.OptionsRequest `json:"options"`
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !decode(w, r, &req, 64<<10) {
		return
	}
	j, err := s.m.Submit(r.Context(), req.URL, req.Options)
	if err != nil {
		if dup, ok := err.(*manager.DuplicateError); ok && dup.Job != nil && dup.Job.ID != 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "jobId": strconv.FormatInt(dup.Job.ID, 10)})
			return
		}
		writeErr(w, err)
		return
	}
	v, _ := s.m.Get(r.Context(), j.ID)
	writeJSON(w, http.StatusCreated, v)
}

type bulkReq struct {
	URLs    []string              `json:"urls"`
	Options config.OptionsRequest `json:"options"`
}

type bulkItem struct {
	URL    string           `json:"url"`
	Status string           `json:"status"` // created | duplicate | error
	Job    *manager.JobView `json:"job,omitempty"`
	JobID  string           `json:"jobId,omitempty"`
	Error  string           `json:"error,omitempty"`
}

func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	var req bulkReq
	if !decode(w, r, &req, 2<<20) {
		return
	}
	if len(req.URLs) > 500 {
		writeError(w, http.StatusBadRequest, "maximum 500 URLs allowed")
		return
	}
	res := s.m.SubmitBulk(r.Context(), req.URLs, req.Options)
	out := struct {
		Created    int        `json:"created"`
		Duplicates int        `json:"duplicates"`
		Errors     int        `json:"errors"`
		Results    []bulkItem `json:"results"`
	}{Results: []bulkItem{}}
	for _, br := range res {
		it := bulkItem{URL: br.URL}
		switch {
		case br.Dup != nil:
			it.Status = "duplicate"
			if br.Dup.ID != 0 {
				it.JobID = strconv.FormatInt(br.Dup.ID, 10)
			}
			out.Duplicates++
		case br.Err != nil:
			it.Status, it.Error = "error", strings.TrimPrefix(br.Err.Error(), manager.ErrInvalid.Error()+": ")
			out.Errors++
		default:
			it.Status = "created"
			v, _ := s.m.Get(r.Context(), br.Job.ID)
			if v != nil {
				v.Items = nil
			}
			it.Job = v
			out.Created++
		}
		out.Results = append(out.Results, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tab := q.Get("state")
	if tab != "" && tab != "active" && tab != "queued" && tab != "completed" && tab != "failed" {
		writeError(w, http.StatusBadRequest, "state must be active, queued, completed or failed")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("cursor"))
	jobs, err := s.m.List(r.Context(), manager.ListFilter{Tab: tab, Query: q.Get("q"), Limit: limit + 1, Offset: offset})
	if err != nil {
		writeErr(w, err)
		return
	}
	next := ""
	if len(jobs) > limit {
		jobs = jobs[:limit]
		next = strconv.Itoa(offset + limit)
	}
	counts, _ := s.m.Counts(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "nextCursor": next, "counts": counts})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := s.m.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.Delete(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.m.GetJob(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.m.Logs.Read(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="job-`+strconv.FormatInt(id, 10)+`.log"`)
	}
	_, _ = w.Write(b)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req manager.RetryRequest
	if !decode(w, r, &req, 64<<10) {
		return
	}
	if req.Options != nil {
		if err := req.Options.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if _, err := s.m.Retry(r.Context(), id, req); err != nil {
		writeErr(w, err)
		return
	}
	v, _ := s.m.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.m.Cancel(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	v, _ := s.m.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleStartNow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.m.StartNow(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

func (s *Server) handleExtras(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	item, ok := pathID(w, r, "item")
	if !ok {
		return
	}
	if err := s.m.RerunExtras(r.Context(), id, item); err != nil {
		if manager.IsNotFound(err) {
			writeErr(w, err)
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	v, _ := s.m.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	n, err := s.m.Clear(r.Context(), r.URL.Query().Get("state"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": n})
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Order []string `json:"order"`
	}
	if !decode(w, r, &req, 256<<10) {
		return
	}
	var ids []int64
	for _, s := range req.Order {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if err := s.m.Reorder(r.Context(), ids); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
