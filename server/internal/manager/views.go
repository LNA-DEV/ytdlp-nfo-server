package manager

import (
	"strconv"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/pipeline"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// JobView is the API representation of a job.
type JobView struct {
	ID           string                `json:"id"`
	URL          string                `json:"url"`
	Title        string                `json:"title"`
	Uploader     string                `json:"uploader"`
	Thumbnail    string                `json:"thumbnail"`
	IsPlaylist   bool                  `json:"isPlaylist"`
	State        store.JobState        `json:"state"`
	Tab          string                `json:"tab"`
	Error        string                `json:"error,omitempty"`
	ErrorClass   string                `json:"errorClass,omitempty"`
	Hint         string                `json:"hint,omitempty"`
	Attempts     int                   `json:"attempts"`
	RetryAt      *time.Time            `json:"retryAt,omitempty"`
	Options      config.Options        `json:"options"`
	Requested    config.OptionsRequest `json:"requested"`
	YtdlpVersion string                `json:"ytdlpVersion,omitempty"`
	CreatedAt    time.Time             `json:"createdAt"`
	StartedAt    *time.Time            `json:"startedAt,omitempty"`
	FinishedAt   *time.Time            `json:"finishedAt,omitempty"`
	QueuePos     int64                 `json:"queuePos"`
	Counts       store.Counts          `json:"counts"`
	Progress     *pipeline.Live        `json:"progress,omitempty"`
	Legacy       bool                  `json:"legacy,omitempty"`
	Items        []ItemView            `json:"items,omitempty"`
}

// ItemView is the API representation of an item.
type ItemView struct {
	ID           string          `json:"id"`
	JobID        string          `json:"jobId"`
	Index        int             `json:"index"`
	URL          string          `json:"url"`
	Title        string          `json:"title"`
	Key          string          `json:"key,omitempty"`
	State        store.ItemState `json:"state"`
	Attempts     int             `json:"attempts"`
	Error        string          `json:"error,omitempty"`
	ErrorClass   string          `json:"errorClass,omitempty"`
	Hint         string          `json:"hint,omitempty"`
	RetryAt      *time.Time      `json:"retryAt,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
	Library      string          `json:"library,omitempty"`
	Path         string          `json:"path,omitempty"`
	Files        []string        `json:"files,omitempty"`
	Size         int64           `json:"size,omitempty"`
	Duration     float64         `json:"duration,omitempty"`
	YtdlpVersion string          `json:"ytdlpVersion,omitempty"`
	FinishedAt   *time.Time      `json:"finishedAt,omitempty"`
}

func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func jobView(j *store.Job, live *pipeline.Live) JobView {
	v := JobView{
		ID: itoa(j.ID), URL: j.URL, Title: j.Title, Uploader: j.Uploader, Thumbnail: j.Thumbnail,
		IsPlaylist: j.IsPlaylist, State: j.State, Tab: j.State.Tab(), Error: j.Error, ErrorClass: j.ErrorClass,
		Hint: j.Hint, Attempts: j.Attempts, RetryAt: tptr(j.RetryAt), Options: j.Options, Requested: j.Requested,
		YtdlpVersion: j.YtdlpVersion, CreatedAt: j.CreatedAt, StartedAt: tptr(j.StartedAt),
		FinishedAt: tptr(j.FinishedAt), QueuePos: j.QueuePos, Counts: j.Counts, Legacy: j.Legacy,
	}
	if live != nil && (j.State == store.JobRunning || j.State == store.JobProbing) {
		l := *live
		v.Progress = &l
	}
	return v
}

func itemView(it *store.Item) ItemView {
	v := ItemView{
		ID: itoa(it.ID), JobID: itoa(it.JobID), Index: it.Index, URL: it.URL, Title: it.Title, Key: it.ArchiveKey,
		State: it.State, Attempts: it.Attempts, Error: it.Error, ErrorClass: it.ErrorClass, Hint: it.Hint,
		RetryAt: tptr(it.RetryAt), Warnings: it.Warnings, Library: it.Library, Path: it.FinalPath,
		Size: it.Size, Duration: it.Duration, YtdlpVersion: it.YtdlpVersion, FinishedAt: tptr(it.FinishedAt),
	}
	if it.State == store.ItemDone {
		for _, f := range it.Files {
			v.Files = append(v.Files, f.Dst)
		}
	}
	return v
}
