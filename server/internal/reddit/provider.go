package reddit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// Provider names.
const (
	ProviderNative      = "native"
	ProviderArcticShift = "arcticshift"
)

// MaxListing is how deep a listing goes (§5.3): Reddit's own limit. Gaps
// beyond it are filled by time (§7.7).
const MaxListing = 1000

// Query asks for posts of a listing.
type Query struct {
	Target Target // a subreddit or user listing
	Limit  int    // posts wanted (at most MaxListing for listings)
	// A time range (Arctic Shift only): posts created after After and
	// before Before; zero values are open ends.
	After, Before time.Time
}

// Page is the result of a listing.
type Page struct {
	Posts []Post // in listing order
	End   bool   // the listing has no more posts
	Title string // display title of the listing if the provider knows one
	Icon  string // avatar or icon URL if known
}

// Provider reads Reddit data (§5, §6).
type Provider interface {
	Name() string
	Site() string // key of the provider's cooldowns in the scheduler
	List(ctx context.Context, q Query) (*Page, error)
	Post(ctx context.Context, id, subreddit string) (*Post, error)
}

// Error is a classified provider error.
type Error struct {
	Class       ytdlp.Class
	Reason      string
	Hint        string
	RetryAfter  time.Duration // a rate limit's reset, if the site said
	Unsupported bool          // the provider can't serve this request at all
	Snippet     string        // the start of an unexpected page, for the log
}

func (e *Error) Error() string { return e.Reason }

// Classification converts the error for the retry policy and the UI.
func (e *Error) Classification() ytdlp.Classification {
	return ytdlp.Classification{Class: e.Class, Reason: e.Reason, Hint: e.Hint, Norm: ytdlp.Normalize(e.Reason)}
}

// fallsThrough reports whether the next provider should be tried (R3).
// temporary reports whether waiting may help: an outage or a rate limit.
func (e *Error) temporary() bool {
	return e.Class == ytdlp.ClassTransient || e.Class == ytdlp.ClassRateLimited
}

func (e *Error) fallsThrough() bool {
	if e.Unsupported {
		return true
	}
	switch e.Class {
	case ytdlp.ClassPermanent:
		return false
	}
	return true
}

// AsError converts any error into a classified one.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Class: ytdlp.ClassTransient, Reason: err.Error()}
	}
	return &Error{Class: ytdlp.ClassTransient, Reason: err.Error()}
}

// Hints shown with provider errors (§5.4).
const (
	HintBlocked     = "Reddit blocks this server's network (VPN or datacenter addresses are often blocked). Use Arctic Shift, or route Reddit through another connection (setting proxy)."
	HintChallenge   = "Reddit answered with a JavaScript check instead of the page, as it does for logged-out visitors. Arctic Shift takes over meanwhile. Turn on \"Answer Reddit's JavaScript check\" in Settings → Reddit."
	HintAuth        = "Reddit shows this community only to logged-in or verified users. Arctic Shift has it if it is enabled."
	HintLayout      = "Reddit's page layout changed; the server needs an update. Run reddit-probe to see the page."
	HintNotFound    = "The subreddit or user does not exist, was banned or is private."
	HintTopNative   = "Top lists need native: Arctic Shift can't rank by score."
	HintNoProviders = "No Reddit provider is enabled. Enable native or Arctic Shift in Settings → Reddit."

	// HintChallengeFailed: the check was answered, but Reddit did not
	// accept the answer, or the check changed.
	HintChallengeFailed = "Reddit's JavaScript check could not be answered; Reddit may have changed it. Arctic Shift takes over meanwhile. reddit-probe --solve shows the page."
)

// Chain tries providers in order (R3). A provider whose site is in a
// cooldown is skipped; a rate limit sets its site's cooldown.
type Chain struct {
	Providers       []Provider
	Cooldown        func(site string) time.Time
	SetCooldown     func(site string, until time.Time)
	DefaultCooldown time.Duration
	// BlockedCooldown pauses a provider that was blocked, so the requests
	// that follow go straight to the next one.
	BlockedCooldown time.Duration
	// OnPause and PauseCause keep the error that paused a provider, so a
	// skipped provider reports it (blocked stays blocked). Optional.
	OnPause    func(site string, cause *Error)
	PauseCause func(site string) *Error
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

func (c *Chain) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Chain) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// run calls fn for each provider until one succeeds or an error doesn't
// fall through. It returns the name of the provider that answered.
func (c *Chain) run(ctx context.Context, fn func(Provider) error) (string, *Error) {
	if len(c.Providers) == 0 {
		return "", &Error{Class: ytdlp.ClassPermanent, Reason: "no Reddit provider enabled", Hint: HintNoProviders}
	}
	var first *Error
	var tried []string
	// temp is the first error that may go away by waiting (an outage, a
	// rate limit): the request is worth retrying whatever the others said.
	var temp *Error
	var tempName string
	for _, p := range c.Providers {
		if ctx.Err() != nil {
			return "", AsError(ctx.Err())
		}
		if c.Cooldown != nil {
			if until := c.Cooldown(p.Site()); until.After(c.now()) {
				e := &Error{Class: ytdlp.ClassRateLimited, Reason: fmt.Sprintf("%s is paused until %s", p.Name(), until.Format("15:04"))}
				if c.PauseCause != nil {
					if cause := c.PauseCause(p.Site()); cause != nil {
						e.Class, e.Hint = cause.Class, cause.Hint
						e.Reason += " after: " + cause.Reason
					}
				}
				c.logf("%s: skipped, %s", p.Name(), e.Reason)
				if first == nil {
					first = e
				}
				if temp == nil && e.temporary() {
					temp, tempName = e, p.Name()
				}
				tried = append(tried, p.Name())
				continue
			}
		}
		err := fn(p)
		if err == nil {
			return p.Name(), nil
		}
		e := AsError(err)
		if ctx.Err() != nil {
			return "", e
		}
		if c.SetCooldown != nil {
			var d time.Duration
			switch e.Class {
			case ytdlp.ClassRateLimited:
				if d = e.RetryAfter; d <= 0 {
					d = c.DefaultCooldown
				}
			case ytdlp.ClassBlocked:
				d = c.BlockedCooldown
			}
			if d > 0 {
				c.SetCooldown(p.Site(), c.now().Add(d))
				if c.OnPause != nil {
					c.OnPause(p.Site(), e)
				}
			}
		}
		if e.Unsupported {
			c.logf("%s: %s", p.Name(), e.Reason)
			if first == nil {
				first = e
			}
		} else {
			c.logf("%s: failed (%s: %s)", p.Name(), e.Class, e.Reason)
			tried = append(tried, p.Name())
			if first == nil || first.Unsupported {
				first = e
			}
			if temp == nil && e.temporary() {
				temp, tempName = e, p.Name()
			}
		}
		if !e.fallsThrough() {
			return "", e
		}
	}
	if first == nil {
		first = &Error{Class: ytdlp.ClassPermanent, Reason: "no provider can serve this request"}
	}
	out := *first
	if temp != nil && temp != first && !first.temporary() {
		// The first error stays the reason; the class says that a retry
		// may get through the other provider.
		out.Class, out.RetryAfter = temp.Class, temp.RetryAfter
		out.Reason = fmt.Sprintf("%s (%s: %s)", first.Reason, tempName, temp.Reason)
	}
	if len(tried) > 1 {
		out.Hint = strings.TrimSpace(out.Hint + " Tried: " + strings.Join(tried, ", ") + ".")
	}
	return "", &out
}

// List lists through the chain.
func (c *Chain) List(ctx context.Context, q Query) (*Page, string, *Error) {
	var page *Page
	name, err := c.run(ctx, func(p Provider) error {
		pg, err := p.List(ctx, q)
		if err == nil {
			page = pg
		}
		return err
	})
	return page, name, err
}

// Post fetches one post through the chain.
func (c *Chain) Post(ctx context.Context, id, subreddit string) (*Post, string, *Error) {
	var post *Post
	name, err := c.run(ctx, func(p Provider) error {
		ps, err := p.Post(ctx, id, subreddit)
		if err == nil {
			post = ps
		}
		return err
	})
	if post != nil {
		post.Provider = name
	}
	return post, name, err
}
