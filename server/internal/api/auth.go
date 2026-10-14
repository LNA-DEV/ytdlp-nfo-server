package api

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
)

const (
	sessionCookie   = "ytdlp_session"
	sessionLifetime = 30 * 24 * time.Hour
	pbkdf2Iter      = 600_000
)

// HashPassword returns a PBKDF2-SHA256 hash for storage in settings.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkHash(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// passwordConfigured reports whether the UI is protected.
func (s *Server) passwordConfigured() bool {
	return s.m.Env.Password != "" || s.m.Settings.Settings().PasswordHash != ""
}

func (s *Server) checkPassword(pw string) bool {
	if pw == "" {
		return false
	}
	if env := s.m.Env.Password; env != "" {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(env)) == 1
	}
	if h := s.m.Settings.Settings().PasswordHash; h != "" {
		return checkHash(pw, h)
	}
	return false
}

type authKind int

const (
	authNone authKind = iota
	authOpen          // no password configured
	authSession
	authToken
	authLegacyPassword
	authProxy
)

func (a authKind) String() string {
	switch a {
	case authOpen:
		return "open"
	case authSession:
		return "session"
	case authToken:
		return "token"
	case authLegacyPassword:
		return "password"
	case authProxy:
		return "proxy"
	}
	return "none"
}

type ctxKey struct{}

// authenticate checks the request. allowQueryToken permits the legacy
// "?token=<password>" used by the v1 UI's EventSource.
func (s *Server) authenticate(r *http.Request, allowQueryToken bool) authKind {
	if h := s.m.Env.TrustedProxyAuthHeader; h != "" && strings.TrimSpace(r.Header.Get(h)) != "" {
		return authProxy
	}
	bearer := ""
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		bearer = strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	if bearer == "" && allowQueryToken {
		bearer = r.URL.Query().Get("token")
	}
	if bearer != "" {
		if strings.HasPrefix(bearer, "ytn_") {
			if _, err := s.m.Store.TokenByHash(r.Context(), manager.HashToken(bearer)); err == nil {
				return authToken
			}
		}
		if s.checkPassword(bearer) {
			s.legacyOnce.Do(func() {
				s.m.SystemWarning("legacy-auth", "A client authenticates with the password as bearer token. This is deprecated: create an API token in Settings → Security.")
			})
			return authLegacyPassword
		}
	}
	// An explicit Authorization header wins over the cookie, so the browser
	// add-on keeps working when the user is also logged into the web UI.
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		ok, _ := s.m.Store.SessionValid(r.Context(), manager.HashToken(c.Value), time.Now().Add(sessionLifetime))
		if ok {
			return authSession
		}
	}
	if !s.passwordConfigured() {
		return authOpen
	}
	return authNone
}

// requireAuth wraps API handlers.
func (s *Server) requireAuth(next http.HandlerFunc, allowQueryToken bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := s.authenticate(r, allowQueryToken)
		if kind == authNone {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Cookie sessions (and an unprotected server) get a CSRF check on
		// state-changing requests; a Bearer header cannot be sent cross-site.
		if (kind == authSession || kind == authOpen) && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, kind)))
	}
}

func sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		for _, ext := range []string{"chrome-extension://", "moz-extension://", "safari-web-extension://"} {
			if strings.HasPrefix(origin, ext) {
				return true // the browser add-on
			}
		}
		o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
		return o == r.Host
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// --- login rate limiting ---

type limiter struct {
	mu    sync.Mutex
	fails map[string]*failState
}

type failState struct {
	count int
	first time.Time
	until time.Time
}

const (
	maxFails   = 5
	failWindow = 15 * time.Minute
	lockFor    = 15 * time.Minute
)

func (l *limiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f, ok := l.fails[ip]; ok && time.Now().Before(f.until) {
		return time.Until(f.until)
	}
	return 0
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	f, ok := l.fails[ip]
	if !ok || now.Sub(f.first) > failWindow {
		f = &failState{first: now}
		l.fails[ip] = f
	}
	f.count++
	if f.count >= maxFails {
		f.until = now.Add(lockFor)
		f.count = 0
		f.first = now
	}
	if len(l.fails) > 10000 { // bound memory
		for k, v := range l.fails {
			if now.Sub(v.first) > failWindow && now.After(v.until) {
				delete(l.fails, k)
			}
		}
	}
}

func (l *limiter) success(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// clientIP returns the client address. Forwarded headers are only trusted
// when the direct peer is a loopback or private address (reverse proxy).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req, 16<<10) {
		return
	}
	ip := clientIP(r)
	if d := s.limiter.blocked(ip); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed logins, try again later")
		return
	}
	if !s.passwordConfigured() {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "authRequired": false})
		return
	}
	if !s.checkPassword(req.Password) {
		s.limiter.fail(ip)
		time.Sleep(300 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.limiter.success(ip)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sid := base64.RawURLEncoding.EncodeToString(b)
	if err := s.m.Store.CreateSession(r.Context(), manager.HashToken(sid), time.Now().Add(sessionLifetime)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: isHTTPS(r), MaxAge: int(sessionLifetime.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "authRequired": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.m.Store.DeleteSession(r.Context(), manager.HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Secure: isHTTPS(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	kind := s.authenticate(r, false)
	writeJSON(w, http.StatusOK, map[string]any{
		"authRequired":    s.passwordConfigured(),
		"authenticated":   kind != authNone,
		"method":          kind.String(),
		"passwordFromEnv": s.m.Env.Password != "",
	})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if s.m.Env.Password != "" {
		writeError(w, http.StatusConflict, "the password is set by the PASSWORD environment variable")
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &req, 16<<10) {
		return
	}
	if s.passwordConfigured() && !s.checkPassword(req.Current) {
		s.limiter.fail(clientIP(r))
		writeError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	hash := ""
	if req.New != "" {
		if len(req.New) < 8 {
			writeError(w, http.StatusBadRequest, "the password must have at least 8 characters")
			return
		}
		var err error
		if hash, err = HashPassword(req.New); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if _, err := s.m.Settings.Update(r.Context(), func(st *settingsT) error { st.PasswordHash = hash; return nil }); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.m.Store.DeleteAllSessions(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"passwordSet": hash != ""})
}
