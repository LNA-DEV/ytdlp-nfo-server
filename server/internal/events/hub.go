// Package events is the global event stream behind GET /api/v1/events
// (§8.2). Events carry IDs so a reconnecting client resumes without losses
// (Last-Event-ID); progress events are ephemeral and not replayed.
package events

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one server-sent event.
type Event struct {
	ID   string // "" for ephemeral events
	Type string
	Data json.RawMessage
}

// Sub is a subscriber.
type Sub struct {
	C       chan Event
	dropped bool
}

// Hub fans out events to subscribers.
type Hub struct {
	mu     sync.Mutex
	epoch  string
	seq    uint64
	ring   []Event
	cap    int
	subs   map[*Sub]struct{}
	closed bool
}

// NewHub creates a hub keeping the last ringSize events for replay.
func NewHub(ringSize int) *Hub {
	return &Hub{
		epoch: strconv.FormatInt(time.Now().UnixNano()/1e6, 36),
		cap:   ringSize,
		subs:  map[*Sub]struct{}{},
	}
}

func marshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	return b
}

// Publish sends a replayable event.
func (h *Hub) Publish(typ string, v any) {
	h.publish(typ, marshal(v), true)
}

// PublishEphemeral sends an event that is not replayed (progress).
func (h *Hub) PublishEphemeral(typ string, v any) {
	h.publish(typ, marshal(v), false)
}

func (h *Hub) publish(typ string, data json.RawMessage, keep bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	e := Event{Type: typ, Data: data}
	if keep {
		h.seq++
		e.ID = h.epoch + ":" + strconv.FormatUint(h.seq, 10)
		h.ring = append(h.ring, e)
		if len(h.ring) > h.cap {
			h.ring = h.ring[len(h.ring)-h.cap:]
		}
	}
	for s := range h.subs {
		select {
		case s.C <- e:
		default:
			// Subscriber too slow: disconnect it; the client reconnects
			// with Last-Event-ID and replays or resyncs.
			if !s.dropped {
				s.dropped = true
				close(s.C)
				delete(h.subs, s)
			}
		}
	}
}

// Subscribe registers a subscriber. If lastID is given, missed events are
// returned for replay; resync is true when they are no longer available
// (the client must reload its state).
func (h *Hub) Subscribe(lastID string) (replay []Event, resync bool, sub *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub = &Sub{C: make(chan Event, 256)}
	if h.closed {
		close(sub.C)
		return nil, false, sub
	}
	h.subs[sub] = struct{}{}
	if lastID == "" {
		return nil, false, sub
	}
	epoch, seqStr, ok := strings.Cut(lastID, ":")
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if !ok || err != nil || epoch != h.epoch {
		return nil, true, sub
	}
	if seq >= h.seq {
		return nil, false, sub
	}
	if len(h.ring) == 0 || seqOf(h.ring[0]) > seq+1 {
		return nil, true, sub
	}
	for _, e := range h.ring {
		if seqOf(e) > seq {
			replay = append(replay, e)
		}
	}
	return replay, false, sub
}

func seqOf(e Event) uint64 {
	_, s, _ := strings.Cut(e.ID, ":")
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// Unsubscribe removes a subscriber.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.C)
	}
}

// Close disconnects everyone (shutdown).
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		close(s.C)
		delete(h.subs, s)
	}
}

// Format renders an event in text/event-stream format.
func Format(e Event) string {
	var b strings.Builder
	if e.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", e.ID)
	}
	if e.Type != "" {
		fmt.Fprintf(&b, "event: %s\n", e.Type)
	}
	for _, line := range strings.Split(string(e.Data), "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	return b.String()
}
