package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// ctl runs jobs until the test finishes them one by one.
type ctl struct {
	mu      sync.Mutex
	started []int64
	finish  map[int64]chan Outcome
}

func newCtl() *ctl { return &ctl{finish: map[int64]chan Outcome{}} }

func (c *ctl) ch(id int64) chan Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finish[id] == nil {
		c.finish[id] = make(chan Outcome, 1)
	}
	return c.finish[id]
}

func (c *ctl) run(ctx context.Context, id int64) Outcome {
	c.mu.Lock()
	c.started = append(c.started, id)
	c.mu.Unlock()
	select {
	case out := <-c.ch(id):
		return out
	case <-ctx.Done():
		return Outcome{Kind: Finished}
	}
}

func (c *ctl) order() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.started...)
}

func (c *ctl) done(id int64, k OutcomeKind) { c.ch(id) <- Outcome{Kind: k} }

func isRunning(s *Scheduler, id int64) bool {
	for _, r := range s.Status().Running {
		if r == id {
			return true
		}
	}
	return false
}

func TestBandsStartInteractiveFirstAndLimitBackground(t *testing.T) {
	c := newCtl()
	s := New(c.run, 1, nil, nil)
	s.Start(context.Background())
	defer s.Shutdown(context.Background())
	s.Pause("test", time.Time{})
	s.Enqueue(Job{ID: 1, Pos: 1, Band: store.BandBacklog})
	s.Enqueue(Job{ID: 2, Pos: 2, Band: store.BandAuto})
	s.Enqueue(Job{ID: 3, Pos: 3, Band: store.BandAuto})
	s.Enqueue(Job{ID: 4, Pos: 4, Band: store.BandInteractive})
	s.Resume()
	waitFor(t, func() bool { return len(c.order()) == 1 })
	if got := c.order(); got[0] != 4 {
		t.Fatalf("started %v, want only the interactive job", got)
	}
	s.SetMax(3)
	s.Enqueue(Job{ID: 4, Pos: 4}) // no-op while running; wakes the loop
	waitFor(t, func() bool { return len(c.order()) >= 2 })
	time.Sleep(20 * time.Millisecond)
	if got := c.order(); len(got) != 2 || got[1] != 2 {
		t.Fatalf("started %v: want the first auto job and nothing else (backgroundSlots = 1)", got)
	}
	if st := s.Status(); st.Background != 1 || st.BackgroundN != 1 {
		t.Fatalf("status %+v", st)
	}
	c.done(2, Finished)
	waitFor(t, func() bool { return isRunning(s, 3) })
	c.done(3, Finished)
	waitFor(t, func() bool { return isRunning(s, 1) }) // backlog last
	s.SetBackgroundSlots(2)
	c.done(4, Finished)
	c.done(1, Finished)
}

func TestBackgroundNotHeldBackByWaitingInteractive(t *testing.T) {
	c := newCtl()
	s := New(c.run, 1, nil, nil)
	s.Start(context.Background())
	defer s.Shutdown(context.Background())
	s.Enqueue(Job{ID: 1, Pos: 1, NotBefore: time.Now().Add(time.Hour)}) // retry backoff
	s.Enqueue(Job{ID: 2, Pos: 2, Band: store.BandAuto})
	waitFor(t, func() bool { return isRunning(s, 2) })
	if s.ShouldYield(2) {
		t.Fatal("must not yield to a job waiting for its backoff")
	}
	c.done(2, Finished)
}

func TestShouldYield(t *testing.T) {
	siteLimit := func(site string) int {
		if site == "one" {
			return 1
		}
		return 0
	}

	t.Run("site limit", func(t *testing.T) {
		c := newCtl()
		s := New(c.run, 3, siteLimit, nil)
		s.Start(context.Background())
		defer s.Shutdown(context.Background())
		s.Enqueue(Job{ID: 1, Pos: 1, Site: "one", Band: store.BandAuto})
		waitFor(t, func() bool { return isRunning(s, 1) })
		s.Enqueue(Job{ID: 2, Pos: 2, Site: "other"})
		waitFor(t, func() bool { return isRunning(s, 2) })
		if s.ShouldYield(1) {
			t.Fatal("an interactive job of another site started anyway")
		}
		s.Enqueue(Job{ID: 3, Pos: 3, Site: "one"})
		if !s.ShouldYield(1) {
			t.Fatal("background job must yield to an interactive job of the same site")
		}
		if s.ShouldYield(2) {
			t.Fatal("interactive jobs never yield")
		}
		// The yielded job goes back to its band; the interactive one runs
		// first, then the background job continues.
		c.done(1, Requeue)
		waitFor(t, func() bool { return isRunning(s, 3) })
		if isRunning(s, 1) {
			t.Fatal("background job restarted before the interactive job")
		}
		c.done(3, Finished)
		waitFor(t, func() bool { return isRunning(s, 1) })
		c.done(1, Finished)
		c.done(2, Finished)
	})

	t.Run("background slots", func(t *testing.T) {
		c := newCtl()
		s := New(c.run, 3, nil, nil)
		s.Start(context.Background())
		defer s.Shutdown(context.Background())
		s.Enqueue(Job{ID: 1, Pos: 1, Band: store.BandBacklog})
		waitFor(t, func() bool { return isRunning(s, 1) })
		s.Enqueue(Job{ID: 2, Pos: 2, Band: store.BandBacklog})
		if s.ShouldYield(1) {
			t.Fatal("must not yield to a job of the same band")
		}
		s.Enqueue(Job{ID: 3, Pos: 3, Band: store.BandAuto})
		if !s.ShouldYield(1) {
			t.Fatal("backlog job must yield to a new auto job")
		}
		c.done(1, Requeue)
		waitFor(t, func() bool { return isRunning(s, 3) })
		if s.ShouldYield(3) {
			t.Fatal("auto job must not yield to backlog jobs")
		}
		c.done(3, Finished)
		waitFor(t, func() bool { return isRunning(s, 1) }) // queue position 1 before 2
		c.done(1, Finished)
		waitFor(t, func() bool { return isRunning(s, 2) })
		c.done(2, Finished)
	})

	t.Run("global limit and forced", func(t *testing.T) {
		c := newCtl()
		s := New(c.run, 1, nil, nil)
		s.Start(context.Background())
		defer s.Shutdown(context.Background())
		s.Enqueue(Job{ID: 1, Pos: 1, Band: store.BandAuto})
		waitFor(t, func() bool { return isRunning(s, 1) })
		s.Pause("test", time.Time{})
		s.Enqueue(Job{ID: 2, Pos: 2, Band: store.BandAuto})
		if !s.StartNow(2) {
			t.Fatal("start now")
		}
		s.Enqueue(Job{ID: 3, Pos: 3})
		if s.ShouldYield(1) {
			t.Fatal("must not yield while paused")
		}
		s.Resume()
		if !s.ShouldYield(1) {
			t.Fatal("background job must yield to an interactive job blocked by the global limit")
		}
		if s.ShouldYield(2) {
			t.Fatal("a job started with StartNow never yields")
		}
		c.done(1, Finished)
		waitFor(t, func() bool { return isRunning(s, 3) })
		c.done(2, Finished)
		c.done(3, Finished)
	})
}
