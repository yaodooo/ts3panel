package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "sid"
	sessionTTL        = 12 * time.Hour
	maxJSONBodyBytes  = 1 << 20
)

var errUnsupportedContentType = errors.New("content type must be application/json")

// sessionManager keeps sessions in memory only; a panel restart intentionally
// logs everyone out. The cookie holds a random opaque sid, never user data.
type sessionManager struct {
	mu   sync.Mutex
	ttl  time.Duration
	byID map[string]sessionRecord
	ops  int
}

type sessionRecord struct {
	username  string
	expiresAt time.Time
}

func newSessionManager() *sessionManager {
	return &sessionManager{
		ttl:  sessionTTL,
		byID: make(map[string]sessionRecord),
	}
}

func (m *sessionManager) Create(username string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	sid := base64.RawURLEncoding.EncodeToString(buf)

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[sid] = sessionRecord{username: username, expiresAt: now.Add(m.ttl)}
	m.ops++
	m.cleanupLocked(now, false)
	return sid, nil
}

func (m *sessionManager) Lookup(sid string, now time.Time) (string, bool) {
	if sid == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.byID[sid]
	if !ok {
		return "", false
	}
	if !now.Before(rec.expiresAt) {
		delete(m.byID, sid)
		return "", false
	}
	rec.expiresAt = now.Add(m.ttl)
	m.byID[sid] = rec
	m.ops++
	m.cleanupLocked(now, false)
	return rec.username, true
}

func (m *sessionManager) Delete(sid string) {
	if sid == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, sid)
}

func (m *sessionManager) DeleteAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID = make(map[string]sessionRecord)
}

func (m *sessionManager) cleanupLocked(now time.Time, force bool) {
	if !force && m.ops < 256 {
		return
	}
	m.ops = 0
	for sid, rec := range m.byID {
		if !now.Before(rec.expiresAt) {
			delete(m.byID, sid)
		}
	}
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, sid string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
		Expires:  time.Now().Add(sessionTTL),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func sessionID(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// attemptLimiter implements fixed-window failure counting with a separate
// lockout period. Login uses 5 failures / 15 minutes / 15 minute lock; the
// forgot-password endpoint uses a higher threshold keyed by source IP.
type attemptLimiter struct {
	mu          sync.Mutex
	max         int
	window      time.Duration
	lockFor     time.Duration
	entries     map[string]*attemptEntry
	lastCleanup time.Time
}

type attemptEntry struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

func newAttemptLimiter(max int, window, lockFor time.Duration) *attemptLimiter {
	return &attemptLimiter{
		max:         max,
		window:      window,
		lockFor:     lockFor,
		entries:     make(map[string]*attemptEntry),
		lastCleanup: time.Now(),
	}
}

// Check reports whether key is currently locked and for how much longer.
func (l *attemptLimiter) Check(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)

	entry, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	if now.Before(entry.lockedUntil) {
		retry := entry.lockedUntil.Sub(now)
		if retry < time.Second {
			retry = time.Second
		}
		return true, retry
	}
	if !entry.lockedUntil.IsZero() {
		// The lockout has expired; start a fresh window on the next failure.
		delete(l.entries, key)
	}
	return false, 0
}

// Fail records a failure and reports whether the key just became locked. The
// fifth failure itself returns locked=true, matching the acceptance contract.
func (l *attemptLimiter) Fail(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)

	entry, ok := l.entries[key]
	if !ok {
		entry = &attemptEntry{windowStart: now}
		l.entries[key] = entry
	}
	if now.Before(entry.lockedUntil) {
		retry := entry.lockedUntil.Sub(now)
		if retry < time.Second {
			retry = time.Second
		}
		return true, retry
	}
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) > l.window {
		entry.failures = 0
		entry.windowStart = now
	}
	entry.failures++
	if entry.failures >= l.max {
		entry.failures = 0
		entry.windowStart = time.Time{}
		entry.lockedUntil = now.Add(l.lockFor)
		return true, l.lockFor
	}
	return false, 0
}

func (l *attemptLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *attemptLimiter) cleanupLocked(now time.Time) {
	if now.Sub(l.lastCleanup) < 5*time.Minute && len(l.entries) < 1024 {
		return
	}
	l.lastCleanup = now
	for key, entry := range l.entries {
		if now.Before(entry.lockedUntil) {
			continue
		}
		if entry.windowStart.IsZero() || now.Sub(entry.windowStart) > l.window {
			delete(l.entries, key)
		}
	}
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write json response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message, hint string) {
	writeJSON(w, status, apiErrorEnvelope{
		Error: apiErrorBody{Code: code, Message: message, Hint: hint},
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if !strings.HasPrefix(ct, "application/json") {
		return errUnsupportedContentType
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
}

func writeRateLimit(w http.ResponseWriter, retry time.Duration) {
	seconds := int(math.Ceil(retry.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, "E_RATE_LIMIT",
		"尝试次数过多，请稍后再试。",
		fmt.Sprintf("请在 %d 秒后重试。", seconds))
}

func clientIP(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	host, _, err := net.SplitHostPort(remote)
	if err == nil && host != "" {
		return host
	}
	if remote == "" {
		return "unknown"
	}
	return remote
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func setupGateExempt(path string) bool {
	switch path {
	case "/api/health",
		"/api/setup",
		"/api/setup/state",
		"/api/setup/skip-dns",
		"/setup",
		"/setup.html",
		"/favicon.ico",
		"/style.css",
		"/app.js",
		"/i18n/zh-CN.json":
		return true
	default:
		return false
	}
}

// setupGate enforces the first-run rule: until setup_completed is true, every
// non-exempt path redirects to /setup. Reads state from disk each time so a
// concurrent setup completion is observed immediately.
func (a *API) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if setupGateExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		st, err := a.state.Snapshot()
		if err != nil {
			a.internalError(w, "setup gate", err)
			return
		}
		if !st.SetupCompleted {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf enforces the custom header on every state-changing API call. Combined
// with SameSite=Strict, this blocks cross-site form/fetch writes.
func (a *API) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWriteMethod(r.Method) && strings.HasPrefix(r.URL.Path, "/api/") {
			if strings.TrimSpace(r.Header.Get("X-Requested-With")) != "ts3panel" {
				writeError(w, http.StatusForbidden, "E_CSRF",
					"请求缺少 X-Requested-With: ts3panel 请求头。",
					"请从面板页面发起请求，或检查代理是否剥离了自定义请求头。")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; "+
				"form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wrote {
		return
	}
	s.status = status
	s.wrote = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func (a *API) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Never log the query string: /setup?token=... contains a one-time
		// credential, and access logs are persisted by journald.
		if r.URL.Path != "/api/health" {
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"ip", clientIP(r),
			)
		}
	})
}

func (a *API) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				if sr, ok := w.(*statusRecorder); ok && sr.wrote {
					return
				}
				writeError(w, http.StatusInternalServerError, "E_INTERNAL",
					"服务器内部错误。", "请查看面板日志。")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireAuth wraps handlers that need a valid session and refreshes the
// sliding cookie expiry on every authenticated request.
func (a *API) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid, ok := sessionID(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "E_AUTH",
				"请先登录。", "会话不存在或已过期。")
			return
		}
		if _, ok := a.sessions.Lookup(sid, time.Now()); !ok {
			clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "E_AUTH",
				"请先登录。", "会话不存在或已过期。")
			return
		}
		setSessionCookie(w, r, sid)
		next(w, r)
	}
}

func (a *API) currentSession(r *http.Request) (string, string, bool) {
	sid, ok := sessionID(r)
	if !ok {
		return "", "", false
	}
	username, ok := a.sessions.Lookup(sid, time.Now())
	if !ok {
		return "", "", false
	}
	return sid, username, true
}

func (a *API) internalError(w http.ResponseWriter, action string, err error) {
	slog.Error("internal error", "action", action, "err", err)
	writeError(w, http.StatusInternalServerError, "E_INTERNAL",
		"服务器内部错误。", "请查看面板日志。")
}

func (a *API) badRequest(w http.ResponseWriter, message string, err error) {
	if errors.Is(err, errUnsupportedContentType) {
		writeError(w, http.StatusUnsupportedMediaType, "E_BAD_REQUEST",
			"请求体必须使用 application/json。", "")
		return
	}
	writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", message, "")
}

// newSession is a small helper shared by setup and login so both paths create
// cookies identically.
func (a *API) newSession(w http.ResponseWriter, r *http.Request, username string) error {
	sid, err := a.sessions.Create(username)
	if err != nil {
		return err
	}
	setSessionCookie(w, r, sid)
	return nil
}
