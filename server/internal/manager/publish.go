package manager

import (
	"context"
	"path/filepath"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/notify"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/pipeline"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// Event types on the global stream.
const (
	EventJob        = "job"
	EventJobRemoved = "job_removed"
	EventItem       = "item"
	EventProgress   = "progress"
	EventCounts     = "counts"
	EventSystem     = "system"
)

// JobChanged implements pipeline.Publisher.
func (m *Manager) JobChanged(j *store.Job) {
	if j.State != store.JobRunning && j.State != store.JobProbing {
		m.mu.Lock()
		delete(m.live, j.ID)
		m.mu.Unlock()
	}
	m.publishJob(j.ID)
}

func (m *Manager) publishJob(id int64) {
	full, err := m.Store.GetJob(context.Background(), id)
	if err != nil {
		return
	}
	m.Hub.Publish(EventJob, m.view(full))
	m.publishCounts()
}

func (m *Manager) publishCounts() {
	if c, err := m.Store.CountJobsByTab(context.Background()); err == nil {
		c["inbox"] = m.Inbox()
		m.Hub.Publish(EventCounts, c)
	}
}

func (m *Manager) view(j *store.Job) JobView {
	m.mu.Lock()
	l, ok := m.live[j.ID]
	m.mu.Unlock()
	if ok {
		return jobView(j, &l)
	}
	return jobView(j, nil)
}

// ItemChanged implements pipeline.Publisher.
func (m *Manager) ItemChanged(it *store.Item) {
	m.Hub.Publish(EventItem, itemView(it))
	if it.State.Terminal() {
		m.itemFinished(it)
	}
}

// Progress implements pipeline.Publisher (already throttled to ≤ 2/s).
func (m *Manager) Progress(l pipeline.Live) {
	m.mu.Lock()
	m.live[l.JobID] = l
	m.mu.Unlock()
	m.Hub.PublishEphemeral(EventProgress, l)
}

// JobFinished implements pipeline.Publisher: notifications.
func (m *Manager) JobFinished(j *store.Job) {
	full, err := m.Store.GetJob(context.Background(), j.ID)
	if err != nil {
		return
	}
	s := m.Settings.Settings()
	info := notify.JobInfo{
		ID: full.ID, URL: full.URL, Title: full.Title, State: string(full.State), Error: full.Error,
		Done: full.Counts.Done, Skipped: full.Counts.Skipped, Failed: full.Counts.Failed, Total: full.Counts.Total,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.Notifier.Send(ctx, s.Notifications, info)
	}()
}

// ItemFinalized implements pipeline.Publisher: Jellyfin refresh.
func (m *Manager) ItemFinalized(j *store.Job, it *store.Item, root string) {
	jf := m.Settings.Settings().Jellyfin
	if jf.URL == "" || jf.APIKey == "" {
		return
	}
	path := filepath.Join(root, filepath.FromSlash(it.FinalPath))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := m.Notifier.JellyfinRefresh(ctx, jf, path); err != nil {
			m.logf("jellyfin: refresh of %s failed: %v", path, err)
			m.Logs.Note(j.ID, "WARNING: Jellyfin refresh failed: %v", err)
		}
	}()
}

// SystemWarning implements pipeline.Publisher.
func (m *Manager) SystemWarning(key, msg string) {
	m.mu.Lock()
	m.warnings[key] = Warning{Key: key, Message: msg, At: time.Now()}
	m.mu.Unlock()
	m.systemChanged()
}

// ClearWarning removes a warning.
func (m *Manager) ClearWarning(key string) {
	m.mu.Lock()
	delete(m.warnings, key)
	m.mu.Unlock()
	m.systemChanged()
}

// systemChanged schedules a "system" event. It never blocks and never calls
// back into the scheduler synchronously (the scheduler invokes it).
func (m *Manager) systemChanged() {
	select {
	case m.sysSignal <- struct{}{}:
	default:
	}
}

func (m *Manager) systemLoop() {
	defer m.bgWG.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.sysSignal:
		}
		m.Hub.Publish(EventSystem, m.QuickStatus(m.ctx))
		// coalesce bursts
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
