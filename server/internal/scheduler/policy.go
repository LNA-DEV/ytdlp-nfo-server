package scheduler

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// Escalations a retry can apply.
const (
	EscalateNone        = ""
	EscalateRefetch     = "refetch"     // re-extract metadata (expired media URL)
	EscalateImpersonate = "impersonate" // add --impersonate=""
	EscalateNightly     = "nightly"     // retry with the newest nightly
)

// Policy is the retry policy (§4.7).
type Policy struct {
	MaxRetries            int           // transient retries
	UnknownBudget         int           // retries for unknown errors (default 2)
	Base                  time.Duration // 30 s
	Cap                   time.Duration // 15 min
	Window                time.Duration // 2 h total
	RateLimitCooldown     time.Duration // 30 min
	EscalateImpersonation bool
	NightlyOnBroken       bool
	Now                   func() time.Time
	Jitter                func() float64 // in [-1, 1]; default random
}

// DefaultPolicy returns the documented defaults.
func DefaultPolicy() Policy {
	return Policy{
		MaxRetries: 3, UnknownBudget: 2, Base: 30 * time.Second, Cap: 15 * time.Minute,
		Window: 2 * time.Hour, RateLimitCooldown: 30 * time.Minute,
		EscalateImpersonation: true, NightlyOnBroken: true,
	}
}

func (p Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Backoff returns 30 s × 2^(n-1) with ±20 % jitter, capped at 15 min.
func (p Policy) Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := p.Base
	for i := 1; i < n && d < p.Cap; i++ {
		d *= 2
	}
	j := 0.0
	if p.Jitter != nil {
		j = p.Jitter()
	} else {
		j = rand.Float64()*2 - 1
	}
	d = time.Duration(float64(d) * (1 + 0.2*j))
	if d > p.Cap {
		d = p.Cap
	}
	return d
}

// Situation is what the policy needs to know about a failed attempt.
type Situation struct {
	Class                  ytdlp.Classification
	Failures               int // failed attempts so far, including this one
	ImpersonationAvailable bool
	Impersonating          bool // the attempt already used impersonation
	NightlyCanHelp         bool // a newer nightly than the one used may exist
}

// Decision is the policy's verdict.
type Decision struct {
	Retry    bool
	Delay    time.Duration
	Escalate string
	Cooldown time.Duration // rate-limited: pause the site this long
	Why      string        // explanation for the log
}

// Decide decides whether and how to retry. It updates rs (the persisted
// retry bookkeeping of the item or job probe).
func (p Policy) Decide(rs *store.RetryState, s Situation) Decision {
	now := p.now()
	if rs.FirstFailureAt.IsZero() {
		rs.FirstFailureAt = now
	}
	c := s.Class
	sameAsLast := rs.LastClass == string(c.Class) && rs.LastNorm == c.Norm && c.Norm != ""
	rs.LastClass, rs.LastNorm = string(c.Class), c.Norm

	switch c.Class {
	case ytdlp.ClassPermanent, ytdlp.ClassAuthRequired:
		return Decision{Why: "this error does not go away by waiting"}
	}
	// Deterministic-error guard: the same class and message twice in a row
	// stops retrying. Transient, rate-limit and blocked failures are
	// excluded: they repeat legitimately and blocked has its own bounded
	// escalation chain.
	if sameAsLast && c.Class != ytdlp.ClassTransient && c.Class != ytdlp.ClassRateLimited && c.Class != ytdlp.ClassBlocked {
		return Decision{Why: "the same error occurred twice in a row"}
	}
	if p.Window > 0 && now.Sub(rs.FirstFailureAt) > p.Window {
		return Decision{Why: fmt.Sprintf("still failing after %s", p.Window)}
	}
	switch c.Class {
	case ytdlp.ClassBlocked:
		if c.MediaForbidden && !rs.Refetched403 {
			rs.Refetched403 = true
			return Decision{Retry: true, Escalate: EscalateRefetch, Why: "re-extracting fresh media URLs"}
		}
		if p.EscalateImpersonation && !rs.Impersonated && !s.Impersonating && s.ImpersonationAvailable {
			rs.Impersonated = true
			return Decision{Retry: true, Escalate: EscalateImpersonate, Why: "retrying with impersonation"}
		}
		if !s.ImpersonationAvailable && !s.Impersonating {
			return Decision{Why: "impersonation is unavailable in this yt-dlp build"}
		}
		return Decision{Why: "access is still denied"}
	case ytdlp.ClassExtractorBroken:
		if p.NightlyOnBroken && !rs.NightlyTried && s.NightlyCanHelp {
			rs.NightlyTried = true
			return Decision{Retry: true, Escalate: EscalateNightly, Why: "retrying with the latest nightly"}
		}
		return Decision{Why: "site support is broken in this yt-dlp version"}
	case ytdlp.ClassCrash:
		if !rs.CrashRetried {
			rs.CrashRetried = true
			return Decision{Retry: true, Delay: p.Backoff(1), Why: "yt-dlp crashed, retrying once"}
		}
		return Decision{Why: "yt-dlp crashed again"}
	case ytdlp.ClassRateLimited:
		return Decision{Retry: true, Delay: p.RateLimitCooldown, Cooldown: p.RateLimitCooldown, Why: "site cooldown"}
	case ytdlp.ClassTransient:
		if s.Failures > p.MaxRetries {
			return Decision{Why: fmt.Sprintf("%d retries exhausted", p.MaxRetries)}
		}
		return Decision{Retry: true, Delay: p.Backoff(s.Failures)}
	default: // unknown
		budget := p.UnknownBudget
		if budget <= 0 || budget > p.MaxRetries {
			budget = min(2, p.MaxRetries)
		}
		if s.Failures > budget {
			return Decision{Why: fmt.Sprintf("%d retries exhausted", budget)}
		}
		return Decision{Retry: true, Delay: p.Backoff(s.Failures)}
	}
}

// LogLine formats the per-attempt log line (F22).
func LogLine(d Decision, s Situation, budget int, at time.Time) string {
	c := s.Class
	what := fmt.Sprintf("%s: %s", c.Class, c.Reason)
	if !d.Retry {
		why := ""
		if d.Why != "" {
			why = " (" + d.Why + ")"
		}
		return fmt.Sprintf("--- Failed (%s). Not retrying%s ---", what, why)
	}
	if d.Escalate != "" || d.Delay == 0 {
		return fmt.Sprintf("--- Attempt %d failed (%s). %s ---", s.Failures, what, capitalize(d.Why))
	}
	retry := fmt.Sprintf("Retry %d/%d", s.Failures, budget)
	if c.Class == ytdlp.ClassRateLimited || c.Class == ytdlp.ClassCrash {
		retry = "Retry"
	}
	return fmt.Sprintf("--- Attempt %d failed (%s). %s at %s ---", s.Failures, what, retry, at.Format("15:04"))
}

func capitalize(s string) string {
	if s == "" {
		return "Retrying now"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
