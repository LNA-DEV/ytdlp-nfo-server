package subscriptions

import (
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// A backfill that failed for a while is repeated after 5 minutes, then
// doubling up to 4 hours, but not before the failing provider's pause ends.
func TestBackfillRetry(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		failures int
		pause    time.Duration
		want     time.Duration
	}{
		{1, 0, 5 * time.Minute},
		{3, 0, 20 * time.Minute},
		{20, 0, 4 * time.Hour},
		{1, 30 * time.Minute, 30 * time.Minute},
	} {
		got := backfillRetry(now, &BackfillResult{Next: &store.Backfill{Failures: c.failures}, RetryAfter: c.pause})
		if got.Sub(now) != c.want {
			t.Errorf("%d failures, pause %v: %v", c.failures, c.pause, got.Sub(now))
		}
	}
}
