// Package scheduler owns the download slots. A single goroutine decides
// which queued jobs start, so the slot count can never drift (F8): a slot is
// released only when the job's goroutine has actually returned.
//
// The queue has three bands (subscriptions concept §4.3): interactive jobs
// start before auto jobs, and those before backlog jobs. Auto and backlog
// jobs (background jobs) together use at most backgroundSlots slots, and
// give their slot back between two items when they block a startable job
// of a higher band (ShouldYield).
package scheduler

import (
	"context"
	"errors"
	"maps"
	"sort"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// Cancellation causes passed to running jobs.
var (
	ErrCancelled = errors.New("cancelled by user")
	ErrShutdown  = errors.New("server shutting down")
	ErrDeleted   = errors.New("job deleted")
)

// OutcomeKind tells the scheduler what to do after a job run returns.
type OutcomeKind int

const (
	// Finished: the job reached a terminal state (or was cancelled).
	Finished OutcomeKind = iota
	// RetryAt: the job waits for a backoff; re-queue it for At.
	RetryAt
	// Requeue: the job could not run now (e.g. low disk space); queue it again.
	Requeue
)

// Outcome is returned by a job run.
type Outcome struct {
	Kind OutcomeKind
	At   time.Time
}

// RunFunc runs one job until it finishes, waits or is cancelled.
type RunFunc func(ctx context.Context, jobID int64) Outcome

// Job is a queue entry.
type Job struct {
	ID        int64
	Pos       int64
	Site      string
	NotBefore time.Time
	Band      store.Band
}

type running struct {
	job    Job
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	forced bool
}

// Status is a snapshot for the UI.
type Status struct {
	Running     []int64              `json:"running"`
	Queued      int                  `json:"queued"`
	Waiting     int                  `json:"waiting"`
	Max         int                  `json:"max"`
	Background  int                  `json:"background"`    // running background jobs
	BackgroundN int                  `json:"backgroundMax"` // backgroundSlots
	Paused      bool                 `json:"paused"`
	PauseReason string               `json:"pauseReason,omitempty"`
	PauseUntil  time.Time            `json:"pauseUntil,omitzero"`
	Cooldowns   map[string]time.Time `json:"cooldowns,omitempty"`
}

// Scheduler runs jobs with a global and per-site concurrency limit.
type Scheduler struct {
	run       RunFunc
	cmds      chan func()
	finished  chan finishedMsg
	stopped   chan struct{}
	onChange  func()
	siteLimit func(site string) int

	// Owned by the loop goroutine.
	max         int
	bgSlots     int
	queue       []Job
	runs        map[int64]*running
	siteRunning map[string]int
	cooldown    map[string]time.Time
	paused      bool
	pauseReason string
	pauseUntil  time.Time
	stopping    bool
	baseCtx     context.Context
}

type finishedMsg struct {
	id  int64
	out Outcome
}

// New creates a scheduler. onChange is called (from the loop goroutine)
// whenever the status changes; siteLimit returns the per-site limit
// (0 = none).
func New(run RunFunc, max int, siteLimit func(string) int, onChange func()) *Scheduler {
	if siteLimit == nil {
		siteLimit = func(string) int { return 0 }
	}
	if onChange == nil {
		onChange = func() {}
	}
	return &Scheduler{
		run:         run,
		cmds:        make(chan func()),
		finished:    make(chan finishedMsg),
		stopped:     make(chan struct{}),
		onChange:    onChange,
		siteLimit:   siteLimit,
		max:         max,
		bgSlots:     1,
		runs:        map[int64]*running{},
		siteRunning: map[string]int{},
		cooldown:    map[string]time.Time{},
	}
}

// Start runs the loop until Shutdown.
func (s *Scheduler) Start(ctx context.Context) {
	s.baseCtx = context.WithoutCancel(ctx)
	go s.loop()
}

// do executes f on the loop goroutine and waits for it. It returns false if
// the scheduler has stopped.
func (s *Scheduler) do(f func()) bool {
	done := make(chan struct{})
	select {
	case s.cmds <- func() { f(); close(done) }:
		<-done
		return true
	case <-s.stopped:
		return false
	}
}

func (s *Scheduler) loop() {
	defer close(s.stopped)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if s.stopping && len(s.runs) == 0 {
			return
		}
		s.dispatch()
		timer.Reset(s.nextWake())
		select {
		case f := <-s.cmds:
			f()
		case m := <-s.finished:
			s.handleFinished(m)
		case <-timer.C:
		}
	}
}

func (s *Scheduler) nextWake() time.Duration {
	now := time.Now()
	next := now.Add(time.Hour)
	consider := func(t time.Time) {
		if t.After(now) && t.Before(next) {
			next = t
		}
	}
	for _, j := range s.queue {
		consider(j.NotBefore)
	}
	for _, t := range s.cooldown {
		consider(t)
	}
	if s.paused && !s.pauseUntil.IsZero() {
		consider(s.pauseUntil)
	}
	return max(time.Until(next), 5*time.Millisecond)
}

func (s *Scheduler) unforcedRunning() int {
	n := 0
	for _, r := range s.runs {
		if !r.forced {
			n++
		}
	}
	return n
}

// backgroundRunning counts the unforced running background jobs.
func (s *Scheduler) backgroundRunning() int {
	n := 0
	for _, r := range s.runs {
		if !r.forced && r.job.Band.Background() {
			n++
		}
	}
	return n
}

// startable reports whether nothing but the slot limits keeps a queued job
// from starting: its NotBefore, a cooldown and its site limit (after
// freeing siteFreed slots of its site).
func (s *Scheduler) startable(j Job, now time.Time, siteFreed int) bool {
	if j.NotBefore.After(now) || s.cooldownActive(j.Site, now) {
		return false
	}
	if lim := s.siteLimit(j.Site); lim > 0 && s.siteRunning[j.Site]-siteFreed >= lim {
		return false
	}
	return true
}

func (s *Scheduler) dispatch() {
	if s.stopping {
		return
	}
	now := time.Now()
	changed := false
	if s.paused && !s.pauseUntil.IsZero() && !now.Before(s.pauseUntil) {
		s.paused, s.pauseReason, s.pauseUntil = false, "", time.Time{}
		changed = true
	}
	for site, until := range s.cooldown {
		if !now.Before(until) {
			delete(s.cooldown, site)
			changed = true
		}
	}
	if !s.paused {
		// The queue is sorted by band, so every startable job of a higher
		// band has been started before a background job is considered.
		for i := 0; i < len(s.queue) && s.unforcedRunning() < s.max; {
			j := s.queue[i]
			blocked := !s.startable(j, now, 0)
			if j.Band.Background() && s.backgroundRunning() >= s.bgSlots {
				blocked = true
			}
			if blocked {
				i++
				continue
			}
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.launch(j, false)
			changed = true
		}
	}
	if changed {
		s.onChange()
	}
}

func (s *Scheduler) cooldownActive(site string, now time.Time) bool {
	until, ok := s.cooldown[site]
	return ok && now.Before(until)
}

func (s *Scheduler) launch(j Job, forced bool) {
	ctx, cancel := context.WithCancelCause(s.baseCtx)
	r := &running{job: j, ctx: ctx, cancel: cancel, done: make(chan struct{}), forced: forced}
	s.runs[j.ID] = r
	s.siteRunning[j.Site]++
	go func() {
		out := Outcome{Kind: Finished}
		defer func() {
			s.finished <- finishedMsg{id: j.ID, out: out}
		}()
		out = s.run(ctx, j.ID)
	}()
}

func (s *Scheduler) handleFinished(m finishedMsg) {
	r, ok := s.runs[m.id]
	if !ok {
		return
	}
	delete(s.runs, m.id)
	if s.siteRunning[r.job.Site]--; s.siteRunning[r.job.Site] <= 0 {
		delete(s.siteRunning, r.job.Site)
	}
	cause := context.Cause(r.ctx)
	r.cancel(nil)
	close(r.done)
	if !s.stopping && cause == nil {
		switch m.out.Kind {
		case RetryAt:
			j := r.job
			j.NotBefore = m.out.At
			s.insert(j)
		case Requeue:
			j := r.job
			j.NotBefore = time.Time{}
			s.insert(j)
		}
	}
	s.onChange()
}

func (s *Scheduler) insert(j Job) {
	for i, q := range s.queue {
		if q.ID == j.ID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			break
		}
	}
	s.queue = append(s.queue, j)
	s.sortQueue()
}

// sortQueue orders the queue by band, then by queue position.
func (s *Scheduler) sortQueue() {
	sort.SliceStable(s.queue, func(a, b int) bool {
		if s.queue[a].Band != s.queue[b].Band {
			return s.queue[a].Band < s.queue[b].Band
		}
		return s.queue[a].Pos < s.queue[b].Pos
	})
}

// --- public API ---

// Enqueue adds (or updates) a job in the queue.
func (s *Scheduler) Enqueue(j Job) {
	s.do(func() {
		if _, running := s.runs[j.ID]; running {
			return
		}
		s.insert(j)
		s.onChange()
	})
}

// Cancel stops a job: a queued job is removed, a running one is cancelled
// with cause. The returned channel closes once the job is no longer running
// (its process group is gone), so its files may then be removed safely.
func (s *Scheduler) Cancel(id int64, cause error) <-chan struct{} {
	done := make(chan struct{})
	ok := s.do(func() {
		for i, q := range s.queue {
			if q.ID == id {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.onChange()
				break
			}
		}
		if r, ok := s.runs[id]; ok {
			r.cancel(cause)
			go func() { <-r.done; close(done) }()
			return
		}
		close(done)
	})
	if !ok {
		close(done)
	}
	return done
}

// StartNow starts a queued job immediately, bypassing the limits.
func (s *Scheduler) StartNow(id int64) bool {
	started := false
	s.do(func() {
		for i, q := range s.queue {
			if q.ID == id {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				q.NotBefore = time.Time{}
				s.launch(q, true)
				started = true
				s.onChange()
				return
			}
		}
	})
	return started
}

// SetPositions updates queue positions (after a reorder).
func (s *Scheduler) SetPositions(pos map[int64]int64) {
	s.do(func() {
		for i := range s.queue {
			if p, ok := pos[s.queue[i].ID]; ok {
				s.queue[i].Pos = p
			}
		}
		s.sortQueue()
		s.onChange()
	})
}

// SetMax changes the global limit.
func (s *Scheduler) SetMax(n int) {
	s.do(func() { s.max = max(n, 1) })
}

// SetBackgroundSlots changes how many background jobs may run at once.
func (s *Scheduler) SetBackgroundSlots(n int) {
	s.do(func() { s.bgSlots = max(n, 1) })
}

// ShouldYield reports whether a running background job blocks a startable
// queued job of a higher band, through the global limit, backgroundSlots
// or its site limit. The job's run then returns Requeue between two items
// and the other job gets the slot. Jobs started with StartNow never yield.
func (s *Scheduler) ShouldYield(id int64) bool {
	yield := false
	s.do(func() {
		r, ok := s.runs[id]
		if !ok || r.forced || !r.job.Band.Background() || s.paused || s.stopping {
			return
		}
		now := time.Now()
		for _, q := range s.queue {
			if q.Band >= r.job.Band {
				break // sorted by band
			}
			freed := 0
			if q.Site == r.job.Site {
				freed = 1
			}
			if !s.startable(q, now, freed) || s.unforcedRunning()-1 >= s.max {
				continue
			}
			if q.Band.Background() && s.backgroundRunning()-1 >= s.bgSlots {
				continue
			}
			// q can start once r is gone; it is blocked by r if it cannot
			// start while r runs.
			blockedNow := !s.startable(q, now, 0) || s.unforcedRunning() >= s.max ||
				q.Band.Background() && s.backgroundRunning() >= s.bgSlots
			if blockedNow {
				yield = true
				return
			}
		}
	})
	return yield
}

// SetCooldown pauses a site until the given time (rate limiting).
func (s *Scheduler) SetCooldown(site string, until time.Time) {
	s.do(func() {
		if until.After(s.cooldown[site]) {
			s.cooldown[site] = until
		}
		s.onChange()
	})
}

// Pause stops starting new jobs until Resume or until (if non-zero).
func (s *Scheduler) Pause(reason string, until time.Time) {
	s.do(func() {
		s.paused, s.pauseReason, s.pauseUntil = true, reason, until
		s.onChange()
	})
}

// Resume lifts a pause.
func (s *Scheduler) Resume() {
	s.do(func() {
		s.paused, s.pauseReason, s.pauseUntil = false, "", time.Time{}
		s.onChange()
	})
}

// IsActive reports whether a job is queued or running.
func (s *Scheduler) IsActive(id int64) bool {
	active := false
	s.do(func() {
		if _, ok := s.runs[id]; ok {
			active = true
			return
		}
		for _, q := range s.queue {
			if q.ID == id {
				active = true
				return
			}
		}
	})
	return active
}

// Status returns a snapshot.
func (s *Scheduler) Status() Status {
	var st Status
	s.do(func() {
		now := time.Now()
		for id := range s.runs {
			st.Running = append(st.Running, id)
		}
		sort.Slice(st.Running, func(a, b int) bool { return st.Running[a] < st.Running[b] })
		for _, q := range s.queue {
			if q.NotBefore.After(now) {
				st.Waiting++
			} else {
				st.Queued++
			}
		}
		st.Max = s.max
		st.Background, st.BackgroundN = s.backgroundRunning(), s.bgSlots
		st.Paused, st.PauseReason, st.PauseUntil = s.paused, s.pauseReason, s.pauseUntil
		if len(s.cooldown) > 0 {
			st.Cooldowns = maps.Clone(s.cooldown)
		}
	})
	return st
}

// Shutdown cancels all running jobs with ErrShutdown and waits for them.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.do(func() {
		s.stopping = true
		for _, r := range s.runs {
			r.cancel(ErrShutdown)
		}
	})
	select {
	case <-s.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
