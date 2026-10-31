package store

import (
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// JobState is the lifecycle state of a job (§4.3).
type JobState string

const (
	JobQueued    JobState = "queued"
	JobProbing   JobState = "probing"
	JobRunning   JobState = "running"
	JobRetrying  JobState = "retrying" // waiting for a backoff, holds no slot
	JobCompleted JobState = "completed"
	JobPartial   JobState = "partial"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

// Terminal reports whether the job is finished.
func (s JobState) Terminal() bool {
	switch s {
	case JobCompleted, JobPartial, JobFailed, JobCancelled:
		return true
	}
	return false
}

// Tab returns the UI tab a job state belongs to.
func (s JobState) Tab() string {
	switch s {
	case JobProbing, JobRunning, JobRetrying:
		return "active"
	case JobQueued:
		return "queued"
	case JobCompleted:
		return "completed"
	default:
		return "failed"
	}
}

// TabStates maps a tab to its job states.
func TabStates(tab string) []JobState {
	switch tab {
	case "active":
		return []JobState{JobProbing, JobRunning, JobRetrying}
	case "queued":
		return []JobState{JobQueued}
	case "completed":
		return []JobState{JobCompleted}
	case "failed":
		return []JobState{JobFailed, JobPartial, JobCancelled}
	}
	return nil
}

// ItemState is the state of one video within a job.
type ItemState string

const (
	ItemPending      ItemState = "pending"
	ItemFetchingMeta ItemState = "fetching_meta"
	ItemDownloading  ItemState = "downloading"
	ItemVerifying    ItemState = "verifying"
	ItemExtras       ItemState = "extras"
	ItemFinalizing   ItemState = "finalizing"
	ItemDone         ItemState = "done"
	ItemSkipped      ItemState = "skipped"
	ItemFailed       ItemState = "failed"
	ItemCancelled    ItemState = "cancelled"
)

// Terminal reports whether the item is finished.
func (s ItemState) Terminal() bool {
	switch s {
	case ItemDone, ItemSkipped, ItemFailed, ItemCancelled:
		return true
	}
	return false
}

// InProgress reports whether the item was being worked on.
func (s ItemState) InProgress() bool {
	switch s {
	case ItemFetchingMeta, ItemDownloading, ItemVerifying, ItemExtras, ItemFinalizing:
		return true
	}
	return false
}

// RetryState tracks the retry bookkeeping of a job probe or an item.
type RetryState struct {
	FirstFailureAt time.Time `json:"firstFailureAt,omitzero"`
	LastClass      string    `json:"lastClass,omitempty"`
	LastNorm       string    `json:"lastNorm,omitempty"`
	Impersonated   bool      `json:"impersonated,omitempty"`
	NightlyTried   bool      `json:"nightlyTried,omitempty"`
	CrashRetried   bool      `json:"crashRetried,omitempty"`
	Refetched403   bool      `json:"refetched403,omitempty"`
}

// Band is a scheduler queue band (subscriptions concept §4.3): jobs of a
// higher band start first, background bands share backgroundSlots.
type Band int

const (
	BandInteractive Band = iota // started by hand, and all jobs outside subscriptions
	BandAuto                    // new videos of subscriptions in auto mode
	BandBacklog                 // backlog downloads of subscriptions
)

// Background reports whether the band is a background band.
func (b Band) Background() bool { return b != BandInteractive }

// String returns the API name of the band.
func (b Band) String() string {
	switch b {
	case BandAuto:
		return "auto"
	case BandBacklog:
		return "backlog"
	}
	return "interactive"
}

// Job is one user submission.
type Job struct {
	ID           int64
	URL          string
	NormURL      string
	Site         string
	Title        string
	Uploader     string
	Thumbnail    string
	IsPlaylist   bool
	State        JobState
	Requested    config.OptionsRequest
	Options      config.Options
	Error        string
	ErrorClass   string
	Hint         string
	Attempts     int
	Retry        RetryState
	RetryAt      time.Time
	QueuePos     int64
	YtdlpVersion string
	CreatedAt    time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	UpdatedAt    time.Time
	Legacy       bool
	// SubscriptionID links a job created by a subscription (0 = none). It
	// is set on creation only: deleting the subscription sets it to NULL.
	SubscriptionID int64
	Band           Band

	Counts Counts // computed when listing
}

// Planned reports whether the job was created with pre-planned items (by
// a subscription). Its URL is a whole channel or playlist, so it must never
// be probed or expanded (subscriptions concept §4.3). Only planned jobs
// have no normalized URL: they are not part of the URL duplicate check.
func (j *Job) Planned() bool { return j.NormURL == "" }

// Counts summarizes the items of a job.
type Counts struct {
	Total    int `json:"total"`
	Done     int `json:"done"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	Pending  int `json:"pending"`
	Warnings int `json:"warnings"`
}

// FileMove maps a file in the work dir to its name in the item folder.
type FileMove struct {
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	Size int64  `json:"size,omitempty"`
	// Media and SHA256 describe a Reddit media file; finalize records them
	// in the media index (Reddit concept §7.5).
	Media  string  `json:"media,omitempty"`
	SHA256 string  `json:"sha256,omitempty"`
	DHash  *uint64 `json:"dhash,omitempty"`
}

// Item is one video of a job.
type Item struct {
	ID            int64
	JobID         int64
	Index         int
	URL           string
	Extractor     string
	VideoID       string
	ArchiveKey    string
	Title         string
	PlaylistTitle string
	State         ItemState
	Attempts      int
	Error         string
	ErrorClass    string
	Hint          string
	Retry         RetryState
	RetryAt       time.Time
	Warnings      []string
	Library       string
	FinalPath     string
	Files         []FileMove
	Size          int64
	Duration      float64
	YtdlpVersion  string
	UpdatedAt     time.Time
	FinishedAt    time.Time
}

// AddWarning appends a warning once.
func (it *Item) AddWarning(w string) {
	for _, x := range it.Warnings {
		if x == w {
			return
		}
	}
	it.Warnings = append(it.Warnings, w)
}

// ArchiveEntry records a finalized video.
type ArchiveEntry struct {
	Key         string    `json:"key"`
	Extractor   string    `json:"extractor"`
	VideoID     string    `json:"videoId"`
	Path        string    `json:"path"`
	JobID       int64     `json:"jobId,omitempty"`
	FinalizedAt time.Time `json:"finalizedAt"`
	Source      string    `json:"source"`
}

// YtdlpVersion is an installed yt-dlp build.
type YtdlpVersion struct {
	ID                 string    `json:"id"`
	Channel            string    `json:"channel"`
	Tag                string    `json:"tag"`
	Version            string    `json:"version"`
	Path               string    `json:"path"`
	Dir                string    `json:"-"`
	Asset              string    `json:"asset"`
	SHA256             string    `json:"sha256"`
	Bundled            bool      `json:"bundled"`
	Pinned             bool      `json:"pinned"`
	SmokeOK            bool      `json:"smokeOk"`
	SmokeOutput        string    `json:"smokeOutput"`
	ImpersonateTargets []string  `json:"impersonateTargets"`
	InstalledAt        time.Time `json:"installedAt"`
}

// CookieProfile is a stored cookies.txt.
type CookieProfile struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Filename  string    `json:"-"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
}

// APIToken is a revocable token (only the hash is stored).
type APIToken struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Hash       string    `json:"-"`
	Prefix     string    `json:"prefix"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitzero"`
}
