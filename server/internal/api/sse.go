package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/events"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
)

func sseHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	return f, true
}

// handleEvents is the global event stream (§8.2): job and item changes,
// throttled progress and system status, resumable via Last-Event-ID.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := sseHeaders(w)
	if !ok {
		return
	}
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("lastEventId")
	}
	replay, resync, sub := s.m.Hub.Subscribe(last)
	defer s.m.Hub.Unsubscribe(sub)
	fmt.Fprint(w, "retry: 3000\n\n")
	if resync {
		fmt.Fprint(w, events.Format(events.Event{Type: "resync", Data: json.RawMessage(`{}`)}))
	}
	for _, e := range replay {
		fmt.Fprint(w, events.Format(e))
	}
	sys, _ := json.Marshal(s.m.QuickStatus(r.Context()))
	fmt.Fprint(w, events.Format(events.Event{Type: manager.EventSystem, Data: sys}))
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, open := <-sub.C:
			if !open {
				return
			}
			fmt.Fprint(w, events.Format(e))
			// Drain what is already queued before flushing.
			for n := 0; n < 64; n++ {
				select {
				case e2, open := <-sub.C:
					if !open {
						flusher.Flush()
						return
					}
					fmt.Fprint(w, events.Format(e2))
					continue
				default:
				}
				break
			}
			flusher.Flush()
		}
	}
}

// handleLogStream streams a job log while a log panel is open.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.m.GetJob(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	flusher, ok := sseHeaders(w)
	if !ok {
		return
	}
	snapshot, ch, cancel := s.m.Logs.Subscribe(id)
	defer cancel()
	data, _ := json.Marshal(string(snapshot))
	fmt.Fprint(w, events.Format(events.Event{Type: "snapshot", Data: data}))
	flusher.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case line, open := <-ch:
			if !open {
				fmt.Fprint(w, events.Format(events.Event{Type: "end", Data: json.RawMessage(`{}`)}))
				flusher.Flush()
				return
			}
			b, _ := json.Marshal(line)
			fmt.Fprint(w, events.Format(events.Event{Type: "line", Data: b}))
			flusher.Flush()
		}
	}
}

// legacyStream serves GET /api/jobs/{id}/stream in the v1 format: log lines
// as plain data, "progress", "status" and a final "done" event.
func (s *Server) legacyStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	job, err := s.m.GetJob(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := sseHeaders(w)
	if !ok {
		return
	}
	writeData := func(line string) {
		for _, l := range strings.Split(line, "\n") {
			fmt.Fprintf(w, "data: %s\n", l)
		}
		fmt.Fprint(w, "\n")
	}
	snapshot, lines, cancel := s.m.Logs.Subscribe(id)
	defer cancel()
	tail := strings.Split(strings.TrimRight(string(snapshot), "\n"), "\n")
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	for _, l := range tail {
		if l != "" {
			writeData(l)
		}
	}
	if job.State.Terminal() {
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", legacyStatus(job.State))
		flusher.Flush()
		return
	}
	_, _, sub := s.m.Hub.Subscribe("")
	defer s.m.Hub.Unsubscribe(sub)
	flusher.Flush()
	lastStatus := legacyStatus(job.State)
	for {
		select {
		case <-r.Context().Done():
			return
		case l, open := <-lines:
			if !open {
				return
			}
			writeData(l)
			flusher.Flush()
		case e, open := <-sub.C:
			if !open {
				return
			}
			switch e.Type {
			case manager.EventProgress:
				var p struct {
					JobID   int64   `json:"jobId"`
					Percent float64 `json:"percent"`
				}
				if json.Unmarshal(e.Data, &p) == nil && p.JobID == id {
					fmt.Fprintf(w, "event: progress\ndata: %.1f\n\n", p.Percent)
					flusher.Flush()
				}
			case manager.EventJob:
				var v manager.JobView
				if json.Unmarshal(e.Data, &v) != nil || v.ID != fmt.Sprint(id) {
					continue
				}
				st := legacyStatus(v.State)
				if v.State.Terminal() {
					fmt.Fprintf(w, "event: done\ndata: %s\n\n", st)
					flusher.Flush()
					return
				}
				if st != lastStatus {
					lastStatus = st
					fmt.Fprintf(w, "event: status\ndata: %s\n\n", st)
					flusher.Flush()
				}
			case manager.EventJobRemoved:
				var v struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(e.Data, &v) == nil && v.ID == fmt.Sprint(id) {
					return
				}
			}
		}
	}
}
