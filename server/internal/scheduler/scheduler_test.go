package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

type harness struct {
	mu       sync.Mutex
	active   int
	peak     int
	perSite  map[string]int
	sitePeak map[string]int
	runs     map[int64]int
	release  chan struct{}
	outcome  func(id int64, n int) Outcome
}

func newHarness() *harness {
	return &harness{perSite: map[string]int{}, sitePeak: map[string]int{}, runs: map[int64]int{}, release: make(chan struct{})}
}

func (h *harness) run(site func(int64) string) RunFunc {
	return func(ctx context.Context, id int64) Outcome {
		h.mu.Lock()
		h.active++
		h.peak = max(h.peak, h.active)
		st := site(id)
		h.perSite[st]++
		h.sitePeak[st] = max(h.sitePeak[st], h.perSite[st])
		h.runs[id]++
		n := h.runs[id]
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.active--
			h.perSite[st]--
			h.mu.Unlock()
		}()
		select {
		case <-h.release:
		case <-ctx.Done():
			return Outcome{Kind: Finished}
		}
		if h.outcome != nil {
			return h.outcome(id, n)
		}
		return Outcome{Kind: Finished}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestGlobalAndSiteLimits(t *testing.T) {
	h := newHarness()
	site := func(id int64) string {
		if id%2 == 0 {
			return "a"
		}
		return "b"
	}
	s := New(h.run(site), 3, func(st string) int {
		if st == "a" {
			return 1
		}
		return 0
	}, nil)
	s.Start(context.Background())
	for i := int64(1); i <= 10; i++ {
		s.Enqueue(Job{ID: i, Pos: i, Site: site(i)})
	}
	waitFor(t, func() bool { return len(s.Status().Running) == 3 })
	close(h.release)
	waitFor(t, func() bool { st := s.Status(); return len(st.Running) == 0 && st.Queued == 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peak > 3 || h.sitePeak["a"] > 1 {
		t.Fatalf("peak=%d sitePeak[a]=%d", h.peak, h.sitePeak["a"])
	}
	for i := int64(1); i <= 10; i++ {
		if h.runs[i] != 1 {
			t.Fatalf("job %d ran %d times", i, h.runs[i])
		}
	}
	s.Shutdown(context.Background())
}

func TestRetryAtRequeues(t *testing.T) {
	h := newHarness()
	close(h.release)
	h.outcome = func(id int64, n int) Outcome {
		if n == 1 {
			return Outcome{Kind: RetryAt, At: time.Now().Add(50 * time.Millisecond)}
		}
		return Outcome{Kind: Finished}
	}
	s := New(h.run(func(int64) string { return "x" }), 1, nil, nil)
	s.Start(context.Background())
	s.Enqueue(Job{ID: 1, Pos: 1})
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.runs[1] == 2 })
	s.Shutdown(context.Background())
}

func TestCancelWaitsAndDoesNotRequeue(t *testing.T) {
	h := newHarness()
	h.outcome = func(int64, int) Outcome { return Outcome{Kind: RetryAt, At: time.Now()} }
	s := New(h.run(func(int64) string { return "x" }), 1, nil, nil)
	s.Start(context.Background())
	s.Enqueue(Job{ID: 1, Pos: 1})
	s.Enqueue(Job{ID: 2, Pos: 2})
	waitFor(t, func() bool { return len(s.Status().Running) == 1 })
	<-s.Cancel(2, ErrCancelled) // queued: removed
	<-s.Cancel(1, ErrCancelled) // running: waits until returned
	h.mu.Lock()
	active := h.active
	h.mu.Unlock()
	if active != 0 {
		t.Fatal("cancel returned before the job stopped")
	}
	time.Sleep(20 * time.Millisecond)
	if st := s.Status(); len(st.Running) != 0 || st.Queued != 0 || st.Waiting != 0 {
		t.Fatalf("status = %+v", st)
	}
	s.Shutdown(context.Background())
}

func TestStartNowAndPause(t *testing.T) {
	h := newHarness()
	s := New(h.run(func(int64) string { return "x" }), 1, nil, nil)
	s.Start(context.Background())
	s.Pause("disk", time.Time{})
	s.Enqueue(Job{ID: 1, Pos: 1})
	s.Enqueue(Job{ID: 2, Pos: 2})
	time.Sleep(20 * time.Millisecond)
	if len(s.Status().Running) != 0 {
		t.Fatal("started while paused")
	}
	if !s.StartNow(2) {
		t.Fatal("StartNow failed")
	}
	s.Resume()
	waitFor(t, func() bool { return len(s.Status().Running) == 2 }) // forced job does not use a slot
	close(h.release)
	s.Shutdown(context.Background())
}

func TestCooldown(t *testing.T) {
	h := newHarness()
	close(h.release)
	s := New(h.run(func(int64) string { return "slow" }), 2, nil, nil)
	s.Start(context.Background())
	s.SetCooldown("slow", time.Now().Add(80*time.Millisecond))
	s.Enqueue(Job{ID: 1, Pos: 1, Site: "slow"})
	time.Sleep(30 * time.Millisecond)
	h.mu.Lock()
	ran := h.runs[1]
	h.mu.Unlock()
	if ran != 0 {
		t.Fatal("started during cooldown")
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.runs[1] == 1 })
	s.Shutdown(context.Background())
}

func TestShutdownCancelsAll(t *testing.T) {
	h := newHarness()
	var cancelled atomic.Int32
	run := func(ctx context.Context, id int64) Outcome {
		<-ctx.Done()
		if context.Cause(ctx) == ErrShutdown {
			cancelled.Add(1)
		}
		return Outcome{Kind: Finished}
	}
	_ = h
	s := New(run, 5, nil, nil)
	s.Start(context.Background())
	for i := int64(1); i <= 5; i++ {
		s.Enqueue(Job{ID: i, Pos: i})
	}
	waitFor(t, func() bool { return len(s.Status().Running) == 5 })
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cancelled.Load() != 5 {
		t.Fatalf("cancelled %d", cancelled.Load())
	}
}

// Delete-all storm: many cancels while jobs start and finish (F8). The
// slot count must never exceed the limit.
func TestCancelStorm(t *testing.T) {
	h := newHarness()
	h.outcome = func(int64, int) Outcome { return Outcome{Kind: Finished} }
	s := New(h.run(func(int64) string { return "x" }), 2, nil, nil)
	s.Start(context.Background())
	var wg sync.WaitGroup
	for round := 0; round < 20; round++ {
		for i := int64(1); i <= 10; i++ {
			s.Enqueue(Job{ID: int64(round)*100 + i, Pos: i})
		}
		for i := int64(1); i <= 10; i++ {
			wg.Add(1)
			go func(id int64) { defer wg.Done(); <-s.Cancel(id, ErrDeleted) }(int64(round)*100 + i)
		}
	}
	wg.Wait()
	h.mu.Lock()
	peak := h.peak
	h.mu.Unlock()
	if peak > 2 {
		t.Fatalf("peak concurrency %d > 2", peak)
	}
	if st := s.Status(); len(st.Running) != 0 {
		t.Fatalf("still running: %v", st.Running)
	}
	s.Shutdown(context.Background())
}

func TestPolicy(t *testing.T) {
	now := time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)
	p := DefaultPolicy()
	p.Now = func() time.Time { return now }
	p.Jitter = func() float64 { return 0 }

	// Backoff: 30 s × 2^(n-1), capped at 15 min.
	if p.Backoff(1) != 30*time.Second || p.Backoff(3) != 2*time.Minute || p.Backoff(10) != 15*time.Minute {
		t.Fatalf("backoff %v %v %v", p.Backoff(1), p.Backoff(3), p.Backoff(10))
	}
	p.Jitter = func() float64 { return 1 }
	if b := p.Backoff(10); b != 15*time.Minute {
		t.Fatalf("jitter exceeded cap: %v", b)
	}
	p.Jitter = func() float64 { return 0 }

	cls := func(c ytdlp.Class, norm string) ytdlp.Classification {
		return ytdlp.Classification{Class: c, Norm: norm, Reason: norm}
	}
	// Permanent: never retried.
	rs := &store.RetryState{}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassPermanent, "private"), Failures: 1}); d.Retry {
		t.Fatal("permanent retried")
	}
	// Errors of the server's own Reddit code get no yt-dlp escalation: no
	// nightly for a changed page, no impersonation for a block.
	for _, c := range []ytdlp.Class{ytdlp.ClassExtractorBroken, ytdlp.ClassBlocked} {
		rs = &store.RetryState{}
		c := ytdlp.Classification{Class: c, Reason: "x", Norm: "x", FromServer: true}
		if d := p.Decide(rs, Situation{Class: c, Failures: 1, NightlyCanHelp: true, ImpersonationAvailable: true}); d.Retry || d.Escalate != "" {
			t.Fatalf("%s from the server: %+v", c.Class, d)
		}
	}
	// Transient: retried up to MaxRetries.
	rs = &store.RetryState{}
	for n := 1; n <= 3; n++ {
		if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassTransient, "reset"), Failures: n}); !d.Retry {
			t.Fatalf("transient attempt %d not retried", n)
		}
	}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassTransient, "reset"), Failures: 4}); d.Retry {
		t.Fatal("transient retried beyond budget")
	}
	// Deterministic guard for unknown errors.
	rs = &store.RetryState{}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassUnknown, "weird"), Failures: 1}); !d.Retry {
		t.Fatal("unknown not retried")
	}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassUnknown, "weird"), Failures: 2}); d.Retry {
		t.Fatal("guard did not stop a repeated unknown error")
	}
	// Total window of 2 h.
	rs = &store.RetryState{FirstFailureAt: now.Add(-3 * time.Hour)}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassTransient, "x"), Failures: 1}); d.Retry {
		t.Fatal("retried after the window")
	}
	// Blocked: refetch on media 403, then exactly one impersonation retry.
	rs = &store.RetryState{}
	media := ytdlp.Classification{Class: ytdlp.ClassBlocked, Norm: "403", MediaForbidden: true}
	if d := p.Decide(rs, Situation{Class: media, Failures: 1, ImpersonationAvailable: true}); d.Escalate != EscalateRefetch {
		t.Fatalf("got %+v", d)
	}
	if d := p.Decide(rs, Situation{Class: media, Failures: 2, ImpersonationAvailable: true}); d.Escalate != EscalateImpersonate {
		t.Fatalf("got %+v", d)
	}
	if d := p.Decide(rs, Situation{Class: media, Failures: 3, ImpersonationAvailable: true, Impersonating: true}); d.Retry {
		t.Fatal("second impersonation retry")
	}
	// Blocked without impersonation support: fail with explanation.
	rs = &store.RetryState{}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassBlocked, "403"), Failures: 1}); d.Retry || d.Why == "" {
		t.Fatalf("got %+v", d)
	}
	// Crash: at most one retry.
	rs = &store.RetryState{}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassCrash, "tb1"), Failures: 1}); !d.Retry {
		t.Fatal("crash not retried once")
	}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassCrash, "tb2"), Failures: 2}); d.Retry {
		t.Fatal("crash retried twice")
	}
	// Extractor broken: one nightly retry.
	rs = &store.RetryState{}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassExtractorBroken, "e"), Failures: 1, NightlyCanHelp: true}); d.Escalate != EscalateNightly {
		t.Fatalf("got %+v", d)
	}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassExtractorBroken, "e2"), Failures: 2, NightlyCanHelp: true}); d.Retry {
		t.Fatal("second nightly retry")
	}
	// Rate limited: cooldown.
	rs = &store.RetryState{}
	if d := p.Decide(rs, Situation{Class: cls(ytdlp.ClassRateLimited, "429"), Failures: 1}); !d.Retry || d.Cooldown != 30*time.Minute {
		t.Fatalf("got %+v", d)
	}
}

func TestLogLine(t *testing.T) {
	at := time.Date(2026, 1, 1, 14, 5, 0, 0, time.UTC)
	s := Situation{Class: ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "connection reset"}, Failures: 2}
	got := LogLine(Decision{Retry: true, Delay: time.Minute}, s, 3, at)
	if got != "--- Attempt 2 failed (transient: connection reset). Retry 2/3 at 14:05 ---" {
		t.Fatal(got)
	}
	s = Situation{Class: ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "video is private"}, Failures: 1}
	got = LogLine(Decision{Why: "this error does not go away by waiting"}, s, 3, at)
	if got != "--- Failed (permanent: video is private). Not retrying (this error does not go away by waiting) ---" {
		t.Fatal(got)
	}
}

// A job enqueued again while its run is still returning (a retry right
// after the job was marked completed) runs again once the run ends.
func TestEnqueueWhileFinishing(t *testing.T) {
	h := newHarness()
	s := New(h.run(func(int64) string { return "x" }), 1, nil, nil)
	s.Start(context.Background())
	s.Enqueue(Job{ID: 1, Pos: 1})
	waitFor(t, func() bool { return len(s.Status().Running) == 1 })
	s.Enqueue(Job{ID: 1, Pos: 2})
	if !s.IsActive(1) {
		t.Fatal("job not active")
	}
	h.release <- struct{}{}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.runs[1] == 2 })
	close(h.release)
	waitFor(t, func() bool { st := s.Status(); return len(st.Running) == 0 && st.Queued == 0 })

	// Cancelled while running: the pending enqueue is dropped as well.
	h2 := newHarness()
	s2 := New(h2.run(func(int64) string { return "x" }), 1, nil, nil)
	s2.Start(context.Background())
	s2.Enqueue(Job{ID: 1, Pos: 1})
	waitFor(t, func() bool { return len(s2.Status().Running) == 1 })
	s2.Enqueue(Job{ID: 1, Pos: 2})
	<-s2.Cancel(1, ErrCancelled)
	time.Sleep(20 * time.Millisecond)
	if st := s2.Status(); len(st.Running) != 0 || st.Queued != 0 || s2.IsActive(1) {
		t.Fatalf("status after cancel: %+v", st)
	}
	s.Shutdown(context.Background())
	s2.Shutdown(context.Background())
}
