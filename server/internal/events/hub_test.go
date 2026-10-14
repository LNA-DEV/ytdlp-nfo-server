package events

import "testing"

func TestReplayAndResync(t *testing.T) {
	h := NewHub(3)
	h.Publish("job", 1)
	h.Publish("job", 2)
	h.PublishEphemeral("progress", 50)
	_, _, s := h.Subscribe("")
	h.Publish("job", 3)
	e := <-s.C
	if e.Type != "job" || string(e.Data) != "3" || e.ID == "" {
		t.Fatalf("got %+v", e)
	}
	first := e.ID
	h.Publish("job", 4)
	replay, resync, _ := h.Subscribe(first)
	if resync || len(replay) != 1 || string(replay[0].Data) != "4" {
		t.Fatalf("replay=%v resync=%v", replay, resync)
	}
	for i := 0; i < 5; i++ {
		h.Publish("job", i)
	}
	if _, resync, _ := h.Subscribe(first); !resync {
		t.Fatal("expected resync after ring overflow")
	}
	if _, resync, _ := h.Subscribe("otherepoch:1"); !resync {
		t.Fatal("expected resync after restart")
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	h := NewHub(10)
	_, _, s := h.Subscribe("")
	for i := 0; i < 300; i++ {
		h.Publish("job", i)
	}
	n := 0
	for range s.C {
		n++
	}
	if n == 0 || n > 256 {
		t.Fatalf("received %d", n)
	}
}
