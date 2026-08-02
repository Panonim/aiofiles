package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"aiofiles/internal/config"
	"aiofiles/internal/proxy"
)

const CookieName = "aiofiles_session"

const (
	// More than maxFailures from one source address within failWindow is
	// rejected outright until the window rolls over.
	maxFailures = 5
	failWindow  = 15 * time.Minute

	// Backstop for an attacker who can vary the source address: the per-IP
	// counter above is keyed on a value derived from the network, so anyone
	// with a subnet to spray from can keep every bucket below maxFailures.
	// Sized well above any plausible household (a handful of people mistyping
	// a handful of times) and short enough to heal itself quickly.
	globalMaxFailures = 100
	globalFailWindow  = 5 * time.Minute

	// The failure map is keyed on a remote-supplied address, so it needs a
	// ceiling of its own between reaps.
	maxFailureEntries = 4096

	// Long on purpose: idle CPU matters here, and expired sessions are also
	// rejected lazily on every request.
	reapInterval = 30 * time.Minute
)

// Each argon2id verification holds 64 MiB, and Login runs one before the
// caller has proved anything, so unbounded concurrent logins are an OOM.
// Small enough to cap the footprint, large enough that a real household never
// queues behind it.
var hashSlots = make(chan struct{}, 4)

var (
	ErrBadCredentials = errors.New("auth: invalid credentials")
	ErrRateLimited    = errors.New("auth: too many failed login attempts")
)

type failCounter struct {
	count int
	first time.Time
}

// Manager keeps sessions in memory (consulted on every request) and writes
// them through to a store when one is attached, so they survive a restart.
// Failure counters are deliberately not persisted: a write per failed login
// costs more than the few seconds a restart hands an attacker.
type Manager struct {
	cfg   *config.Config
	ttl   time.Duration
	trust *proxy.Trust

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	failures map[string]*failCounter
	global   failCounter  // across all source addresses
	store    SessionStore // nil when sessions are memory-only
}

func NewManager(cfg *config.Config) *Manager {
	ttl := time.Duration(cfg.SessionTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 720 * time.Hour
	}
	return &Manager{
		cfg:      cfg,
		ttl:      ttl,
		trust:    proxy.New(cfg.TrustedProxies),
		sessions: make(map[string]time.Time),
		failures: make(map[string]*failCounter),
	}
}

// Persist attaches a store and adopts the live sessions it already holds. An
// error is worth surfacing - sessions will not survive the next restart - but
// is not fatal: the manager keeps working in memory.
func (m *Manager) Persist(store SessionStore) error {
	if store == nil {
		return nil
	}

	// Sessions are bound to the credential that issued them: rotating
	// AUTH_PASSWORD_HASH after a compromise has to invalidate cookies handed
	// out under the old one. Re-hashing draws a fresh salt, so even the same
	// password produces a different string and a different fingerprint.
	if err := store.Rebind(credentialFingerprint(m.cfg.AuthPasswordHash)); err != nil {
		m.mu.Lock()
		m.store = store
		m.mu.Unlock()
		return err // fail closed: adopt no session we cannot vouch for
	}

	now := time.Now()
	live, err := store.Live(now)

	m.mu.Lock()
	m.store = store
	for token, exp := range live {
		m.sessions[token] = exp
	}
	m.mu.Unlock()

	if err != nil {
		return err
	}
	return store.DeleteExpired(now)
}

func (m *Manager) Enabled() bool { return m.cfg.AuthEnabled() }

func (m *Manager) Username() string { return m.cfg.AuthUsername }

// Login needs the request for the rate-limiting source address and for whether
// the connection is TLS - an unconditional Secure cookie would silently break
// login on a plain-HTTP LAN box.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request, username, password string) error {
	if !m.cfg.AuthEnabled() {
		return nil // nothing to log into
	}

	ip := m.trust.ClientIP(r)
	if m.rateLimited(ip) {
		return ErrRateLimited
	}

	// Hashed unconditionally, including for an unknown username, so the two
	// failure modes take the same time.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(m.cfg.AuthUsername)) == 1
	passOK, err := m.verifyPassword(r.Context(), password)
	if err != nil {
		return err
	}
	if !userOK || !passOK {
		m.recordFailure(ip)
		return ErrBadCredentials
	}

	token, err := newToken()
	if err != nil {
		return err
	}
	expiry := time.Now().Add(m.ttl)

	m.mu.Lock()
	m.sessions[token] = expiry
	delete(m.failures, ip)
	m.global = failCounter{} // a real login clears the household's backstop
	store := m.store
	m.mu.Unlock()

	// A failed write only costs this session its survival across a restart, so
	// it must not cost the user the login they just completed.
	if store != nil {
		if err := store.Save(token, expiry); err != nil {
			slog.Default().Warn("persist session", "err", err)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		MaxAge:   int(m.ttl / time.Second),
		HttpOnly: true,
		Secure:   m.trust.IsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		store := m.store
		m.mu.Unlock()

		if store != nil {
			if err := store.Delete(c.Value); err != nil {
				slog.Default().Warn("drop persisted session", "err", err)
			}
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.trust.IsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// Authenticated is always true when auth is disabled.
func (m *Manager) Authenticated(r *http.Request) bool {
	if !m.cfg.AuthEnabled() {
		return true
	}
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return false
	}

	m.mu.Lock()
	exp, ok := m.sessions[c.Value]
	expired := ok && time.Now().After(exp)
	if expired {
		delete(m.sessions, c.Value)
	}
	store := m.store
	m.mu.Unlock()

	if expired && store != nil {
		if err := store.Delete(c.Value); err != nil {
			slog.Default().Warn("drop expired session", "err", err)
		}
	}
	return ok && !expired
}

// Middleware rejects unauthenticated requests with a JSON 401.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.cfg.AuthEnabled() || m.Authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"authentication required"}}`))
	})
}

// StartReaper evicts expired sessions and stale failure counters in the
// background and returns immediately.
func (m *Manager) StartReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				m.reap(now)
			}
		}
	}()
}

func (m *Manager) reap(now time.Time) {
	m.mu.Lock()
	for token, exp := range m.sessions {
		if now.After(exp) {
			delete(m.sessions, token)
		}
	}
	m.pruneFailuresLocked(now)
	store := m.store
	m.mu.Unlock()

	if store != nil {
		if err := store.DeleteExpired(now); err != nil {
			slog.Default().Warn("reap persisted sessions", "err", err)
		}
	}
}

// verifyPassword runs the argon2id verification under the global concurrency
// cap. A caller that goes away while waiting is shed rather than queued, so a
// flood of logins cannot pile up 64 MiB allocations; ErrRateLimited keeps that
// a 429 instead of a logged 500 on every client disconnect.
func (m *Manager) verifyPassword(ctx context.Context, password string) (bool, error) {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return false, ErrRateLimited
	}
	defer func() { <-hashSlots }()
	return VerifyPassword(m.cfg.AuthPasswordHash, password)
}

func (m *Manager) rateLimited(ip string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()

	if m.global.count > 0 && now.Sub(m.global.first) > globalFailWindow {
		m.global = failCounter{}
	}
	if m.global.count >= globalMaxFailures {
		return true
	}

	f, ok := m.failures[ip]
	if !ok {
		return false
	}
	if now.Sub(f.first) > failWindow {
		delete(m.failures, ip)
		return false
	}
	return f.count >= maxFailures
}

func (m *Manager) recordFailure(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()

	if m.global.count == 0 || now.Sub(m.global.first) > globalFailWindow {
		m.global = failCounter{count: 1, first: now}
	} else {
		m.global.count++
	}

	if f, ok := m.failures[ip]; ok {
		if now.Sub(f.first) > failWindow {
			*f = failCounter{count: 1, first: now}
		} else {
			f.count++
		}
		return
	}
	if len(m.failures) >= maxFailureEntries {
		m.pruneFailuresLocked(now)
		// Still full: stop tracking new addresses rather than grow without
		// bound. The global counter above is what catches this case anyway.
		if len(m.failures) >= maxFailureEntries {
			return
		}
	}
	m.failures[ip] = &failCounter{count: 1, first: now}
}

func (m *Manager) pruneFailuresLocked(now time.Time) {
	for ip, f := range m.failures {
		if now.Sub(f.first) > failWindow {
			delete(m.failures, ip)
		}
	}
}

// credentialFingerprint identifies the credential a session was issued under
// without storing the hash itself; truncated because it only ever needs to
// detect change, not resist preimage search.
func credentialFingerprint(passwordHash string) string {
	sum := sha256.Sum256([]byte(passwordHash))
	return hex.EncodeToString(sum[:8])
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ClientIP is the address the rate limiter counts against; see internal/proxy
// for how far into X-Forwarded-For it is willing to look.
func (m *Manager) ClientIP(r *http.Request) string { return m.trust.ClientIP(r) }
