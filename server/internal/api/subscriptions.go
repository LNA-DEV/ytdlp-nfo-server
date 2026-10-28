package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// writeSubErr maps subscription errors: 409 with the existing
// subscription, 422 for URLs that cannot be subscribed to.
func writeSubErr(w http.ResponseWriter, err error) {
	var c *manager.SubscriptionConflict
	var pe *manager.PreviewError
	switch {
	case errors.As(err, &c):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(),
			"subscriptionId": strconv.FormatInt(c.SubscriptionID, 10), "name": c.Name})
	case errors.Is(err, manager.ErrUnprocessable):
		writeError(w, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), manager.ErrUnprocessable.Error()+": "))
	case errors.As(err, &pe):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": pe.Class.Reason,
			"class": string(pe.Class.Class), "hint": pe.Class.Hint})
	default:
		writeErr(w, err)
	}
}

func (s *Server) handleSubPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req, 16<<10) {
		return
	}
	p, err := s.m.PreviewSubscription(r.Context(), req.URL)
	if err != nil {
		writeSubErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleListSubs(w http.ResponseWriter, r *http.Request) {
	subs, err := s.m.ListSubscriptions(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs, "inbox": s.m.Inbox()})
}

func (s *Server) handleCreateSub(w http.ResponseWriter, r *http.Request) {
	var in manager.SubscriptionInput
	if !decode(w, r, &in, 256<<10) {
		return
	}
	v, err := s.m.CreateSubscription(r.Context(), in)
	if err != nil {
		writeSubErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleGetSub(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := s.m.GetSubscription(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handlePatchSub(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in manager.SubscriptionInput
	if !decode(w, r, &in, 256<<10) {
		return
	}
	v, err := s.m.UpdateSubscription(r.Context(), id, in)
	if err != nil {
		writeSubErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteSub(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	cancel := r.URL.Query().Get("cancelQueued")
	if err := s.m.DeleteSubscription(r.Context(), id, cancel == "1" || cancel == "true"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleCheckSub(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Full bool `json:"full"`
	}
	if !decode(w, r, &req, 4<<10) {
		return
	}
	if err := s.m.CheckSubscription(r.Context(), id, req.Full); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleCheckAll makes all (or the listed) enabled subscriptions due now;
// meant for an external cron (S10).
func (s *Server) handleCheckAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &req, 64<<10) {
		return
	}
	var ids []int64
	for _, v := range req.IDs {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid id "+strconv.Quote(v))
			return
		}
		ids = append(ids, id)
	}
	n, err := s.m.CheckSubscriptions(r.Context(), ids)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"due": n})
}

func (s *Server) handleSubLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.m.GetSubscription(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.m.SubscriptionLogs().Read(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

const entryPage = 50

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request, subID int64) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	if subID == 0 && q.Get("subscription") != "" {
		id, err := strconv.ParseInt(q.Get("subscription"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid subscription")
			return
		}
		subID = id
	}
	status := q.Get("status")
	if !store.ValidEntryStatus(status) {
		writeError(w, http.StatusBadRequest, "unknown status "+strconv.Quote(status))
		return
	}
	es, err := s.m.ListEntries(r.Context(), manager.EntryListFilter{SubscriptionID: subID, Status: status,
		Query: q.Get("q"), Limit: entryPage + 1, Offset: max(offset, 0)})
	if err != nil {
		writeErr(w, err)
		return
	}
	next := ""
	if len(es) > entryPage {
		es = es[:entryPage]
		next = strconv.Itoa(max(offset, 0) + entryPage)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": es, "nextOffset": next})
}

func (s *Server) handleSubEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.listEntries(w, r, id)
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) { s.listEntries(w, r, 0) }

func (s *Server) handleDownloadEntries(w http.ResponseWriter, r *http.Request) {
	var req manager.DownloadEntriesRequest
	if !decode(w, r, &req, 256<<10) {
		return
	}
	res, err := s.m.DownloadEntries(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleImportSubs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Format string `json:"format"`
		Data   string `json:"data"`
	}
	if !decode(w, r, &req, 4<<20) {
		return
	}
	res, err := s.m.ImportSubscriptions(r.Context(), req.Format, req.Data)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleExportSubs(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	b, err := s.m.ExportSubscriptions(r.Context(), format)
	if err != nil {
		writeErr(w, err)
		return
	}
	name, ctype := "subscriptions.txt", "text/plain; charset=utf-8"
	if format == manager.FormatOPML {
		name, ctype = "subscriptions.opml", "text/x-opml; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(b)
}

func (s *Server) handleIgnoreEntries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs     []string `json:"ids"`
		Ignored *bool    `json:"ignored"`
	}
	if !decode(w, r, &req, 256<<10) {
		return
	}
	if req.Ignored == nil {
		writeError(w, http.StatusBadRequest, "ignored is required")
		return
	}
	n, err := s.m.IgnoreEntries(r.Context(), req.IDs, *req.Ignored)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"changed": n})
}
