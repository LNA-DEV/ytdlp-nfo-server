package manager

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// SettingsService holds the runtime settings (env-locked values applied).
type SettingsService struct {
	mu       sync.RWMutex
	cur      config.Settings
	st       *store.Store
	env      *config.Env
	onChange func(old, new config.Settings)
}

func clone(s config.Settings) config.Settings {
	b, _ := json.Marshal(s)
	var out config.Settings
	_ = json.Unmarshal(b, &out)
	out.Locked = append([]string(nil), s.Locked...)
	out.PasswordHash = s.PasswordHash
	return out
}

// Settings returns a copy of the current settings.
func (s *SettingsService) Settings() config.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cur)
}

// Update applies fn to a copy, re-applies environment locks, validates,
// persists and publishes the result.
func (s *SettingsService) Update(ctx context.Context, fn func(*config.Settings) error) (config.Settings, error) {
	s.mu.Lock()
	old := clone(s.cur)
	next := clone(s.cur)
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return old, err
	}
	next.Normalize()
	next.ApplyEnv(s.env)
	if err := next.Validate(); err != nil {
		s.mu.Unlock()
		return old, err
	}
	if err := s.st.SaveSettings(ctx, next); err != nil {
		s.mu.Unlock()
		return old, err
	}
	s.cur = next
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb(old, clone(next))
	}
	return clone(next), nil
}

// UpdateYtdlp implements versions.SettingsAccess. A channel fixed by
// YTDLP_CHANNEL is kept.
func (s *SettingsService) UpdateYtdlp(ctx context.Context, fn func(*config.YtdlpSettings)) error {
	_, err := s.Update(ctx, func(st *config.Settings) error {
		ch := st.Ytdlp.Channel
		fn(&st.Ytdlp)
		if st.IsLocked("ytdlp.channel") {
			st.Ytdlp.Channel = ch
		}
		return nil
	})
	return err
}
