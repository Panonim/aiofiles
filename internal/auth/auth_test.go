package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"aiofiles/internal/config"
	"aiofiles/internal/db"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const plain = "correct horse battery staple"

	hash, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("unexpected PHC prefix: %q", hash)
	}

	ok, err := VerifyPassword(hash, plain)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}

	for _, wrong := range []string{"", "correct horse battery stapl", "Correct Horse Battery Staple", plain + " "} {
		ok, err := VerifyPassword(hash, wrong)
		if err != nil {
			t.Errorf("VerifyPassword(%q) errored: %v", wrong, err)
		}
		if ok {
			t.Errorf("VerifyPassword accepted %q", wrong)
		}
	}

	second, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if second == hash {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
}

// A broken AUTH_PASSWORD_HASH must fail loudly rather than panic or silently
// authenticate.
func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	good, err := HashPassword("x")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(good, "$")

	tests := []struct {
		name    string
		hash    string
		wantErr error
	}{
		{"empty", "", ErrInvalidHash},
		{"not a hash at all", "hunter2", ErrInvalidHash},
		{"bcrypt", "$2y$10$abcdefghijklmnopqrstuv", ErrInvalidHash},
		{"wrong algorithm", "$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2g", ErrInvalidHash},
		{"missing leading separator", strings.TrimPrefix(good, "$"), ErrInvalidHash},
		{"truncated", strings.Join(parts[:5], "$"), ErrInvalidHash},
		{"extra field", good + "$extra", ErrInvalidHash},
		{"unparseable version", "$argon2id$vNINETEEN$m=65536,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2g", ErrInvalidHash},
		{"future version", "$argon2id$v=99$m=65536,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2g", ErrIncompatibleVersion},
		{"unparseable params", "$argon2id$v=19$memory=65536$c2FsdHNhbHQ$aGFzaGhhc2g", ErrInvalidHash},
		{"zero memory", "$argon2id$v=19$m=0,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2g", ErrInvalidHash},
		{"zero time", "$argon2id$v=19$m=65536,t=0,p=2$c2FsdHNhbHQ$aGFzaGhhc2g", ErrInvalidHash},
		{"zero threads", "$argon2id$v=19$m=65536,t=3,p=0$c2FsdHNhbHQ$aGFzaGhhc2g", ErrInvalidHash},
		{"salt is not base64", "$argon2id$v=19$m=65536,t=3,p=2$!!!!$aGFzaGhhc2g", ErrInvalidHash},
		{"empty salt", "$argon2id$v=19$m=65536,t=3,p=2$$aGFzaGhhc2g", ErrInvalidHash},
		{"key is not base64", "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$!!!!", ErrInvalidHash},
		{"empty key", "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$", ErrInvalidHash},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.hash, "anything")
			if ok {
				t.Error("a malformed hash authenticated")
			}
			if err != tc.wantErr {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

const testPassword = "s3cret-password"

// The hash is computed once: argon2id with 64 MiB is deliberately slow.
var hashOnce = sync.OnceValues(func() (string, error) { return HashPassword(testPassword) })

func hashForTests(t *testing.T) string {
	t.Helper()
	h, err := hashOnce()
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

func enabledConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AuthUsername: "admin", AuthPasswordHash: hashForTests(t), SessionTTLHours: 1}
}

// cheapHash is a real PHC string with deliberately tiny argon2id parameters -
// VerifyPassword takes them from the hash itself - so tests that need hundreds
// of verifications do not pay 64 MiB each time.
func cheapHash(t *testing.T, plain string) string {
	t.Helper()
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("salt: %v", err)
	}
	key := argon2.IDKey([]byte(plain), salt, 1, 8, 1, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=8,t=1,p=1$%s$%s",
		argon2.Version, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func cheapConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AuthUsername: "admin", AuthPasswordHash: cheapHash(t, testPassword), SessionTTLHours: 1}
}

// login performs a login and returns the session cookie it issued.
func login(t *testing.T, m *Manager, user, pass string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	if err := m.Login(rec, req, user, pass); err != nil {
		t.Fatalf("Login: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatal("login issued no session cookie")
	return nil
}

func requestWith(c *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	if c != nil {
		r.AddCookie(c)
	}
	return r
}

func TestManagerSessionLifecycle(t *testing.T) {
	m := NewManager(enabledConfig(t))
	if !m.Enabled() {
		t.Fatal("auth should be enabled")
	}
	if m.Authenticated(requestWith(nil)) {
		t.Error("a request without a cookie authenticated")
	}

	cookie := login(t, m, "admin", testPassword)
	if cookie.Value == "" || !cookie.HttpOnly || cookie.Secure {
		t.Errorf("cookie = %+v; want a non-empty HttpOnly, non-Secure (plain HTTP) cookie", cookie)
	}
	if !m.Authenticated(requestWith(cookie)) {
		t.Fatal("the session cookie did not authenticate")
	}

	if m.Authenticated(requestWith(&http.Cookie{Name: CookieName, Value: "forged"})) {
		t.Error("a forged token authenticated")
	}

	rec := httptest.NewRecorder()
	m.Logout(rec, requestWith(cookie))
	if m.Authenticated(requestWith(cookie)) {
		t.Error("the session survived logout")
	}
}

func TestManagerRejectsBadCredentials(t *testing.T) {
	tests := []struct{ name, user, pass string }{
		{"wrong password", "admin", "not the password"},
		{"empty password", "admin", ""},
		{"wrong username", "root", testPassword},
		{"both wrong", "root", "nope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(enabledConfig(t))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			req.RemoteAddr = "10.0.0.1:1234"

			if err := m.Login(rec, req, tc.user, tc.pass); err != ErrBadCredentials {
				t.Fatalf("Login err = %v, want ErrBadCredentials", err)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Error("a failed login issued a cookie")
			}
		})
	}
}

func TestManagerRateLimitsRepeatedFailures(t *testing.T) {
	m := NewManager(enabledConfig(t))
	attempt := func(pass string) error {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		return m.Login(httptest.NewRecorder(), req, "admin", pass)
	}

	for i := range maxFailures {
		if err := attempt("wrong"); err != ErrBadCredentials {
			t.Fatalf("attempt %d: err = %v, want ErrBadCredentials", i, err)
		}
	}
	if err := attempt("wrong"); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited after %d failures", err, maxFailures)
	}
	// The correct password must not get through the lockout either.
	if err := attempt(testPassword); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	// A different source address is unaffected.
	other := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	other.RemoteAddr = "10.0.0.10:1234"
	if err := m.Login(httptest.NewRecorder(), other, "admin", testPassword); err != nil {
		t.Fatalf("second address was locked out too: %v", err)
	}
}

func TestManagerExpiredSessionIsRejected(t *testing.T) {
	m := NewManager(enabledConfig(t))
	cookie := login(t, m, "admin", testPassword)

	m.reap(time.Now().Add(2 * time.Hour))
	if m.Authenticated(requestWith(cookie)) {
		t.Error("an expired session still authenticated")
	}
}

func TestManagerDisabledAuthLetsEverythingThrough(t *testing.T) {
	m := NewManager(&config.Config{})
	if m.Enabled() {
		t.Fatal("auth should be disabled with no username or hash")
	}
	if !m.Authenticated(requestWith(nil)) {
		t.Error("requests should pass when auth is disabled")
	}
	if err := m.Login(httptest.NewRecorder(), requestWith(nil), "", ""); err != nil {
		t.Errorf("Login with auth disabled = %v, want nil", err)
	}

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	rec := httptest.NewRecorder()
	m.Middleware(next).ServeHTTP(rec, requestWith(nil))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("middleware blocked a request with auth disabled (called=%v, code=%d)", called, rec.Code)
	}
}

func TestMiddlewareRejectsUnauthenticatedWithJSON(t *testing.T) {
	m := NewManager(enabledConfig(t))
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the protected handler ran without a session")
	})

	rec := httptest.NewRecorder()
	m.Middleware(next).ServeHTTP(rec, requestWith(nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q, want JSON", ct)
	}
	if !strings.Contains(rec.Body.String(), `"unauthorized"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	handle, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return handle
}

func TestSQLSessionStore(t *testing.T) {
	store := NewSQLSessionStore(openDB(t))
	now := time.Now()

	if err := store.Save("live", now.Add(time.Hour)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save("stale", now.Add(-time.Hour)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	live, err := store.Live(now)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if _, ok := live["live"]; !ok {
		t.Error("Live omitted an unexpired session")
	}
	if _, ok := live["stale"]; ok {
		t.Error("Live returned an expired session")
	}

	// Re-saving the same token extends it rather than failing on the primary key.
	if err := store.Save("live", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("re-Save: %v", err)
	}

	if err := store.DeleteExpired(now); err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if err := store.Delete("live"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	live, err = store.Live(now)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("sessions remain after deletion: %v", live)
	}
}

// A restart must not sign everyone out.
func TestSessionsSurviveAcrossManagers(t *testing.T) {
	store := NewSQLSessionStore(openDB(t))
	cfg := enabledConfig(t)

	first := NewManager(cfg)
	if err := first.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	cookie := login(t, first, "admin", testPassword)

	second := NewManager(cfg)
	if err := second.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if !second.Authenticated(requestWith(cookie)) {
		t.Fatal("the session did not survive a restart")
	}

	rec := httptest.NewRecorder()
	second.Logout(rec, requestWith(cookie))

	third := NewManager(cfg)
	if err := third.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if third.Authenticated(requestWith(cookie)) {
		t.Error("a logged-out session came back after a restart")
	}
}

// The chain-walking itself is covered in internal/proxy; what matters here is
// that the Manager keys its rate limiter on the client rather than on the proxy
// in front of it.
func TestClientIP(t *testing.T) {
	m := NewManager(cheapConfig(t))
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{"direct client", "192.0.2.5:4321", nil, "192.0.2.5"},
		// The bundled nginx is on loopback and appends the peer it saw.
		{"behind the bundled nginx", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "203.0.113.7"}, "203.0.113.7"},
		{"spoofed leading element is ignored", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.7"}, "203.0.113.7"},
		{"headers from an untrusted peer are ignored", "192.0.2.5:4321",
			map[string]string{"X-Forwarded-For": "203.0.113.7", "X-Real-IP": "203.0.113.9"}, "192.0.2.5"},
		{"real ip", "127.0.0.1:1", map[string]string{"X-Real-IP": " 203.0.113.9 "}, "203.0.113.9"},
		{"forwarded for wins", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.1", "X-Real-IP": "203.0.113.9"}, "198.51.100.1"},
		{"blank forwarded for falls back", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "  ", "X-Real-IP": "203.0.113.9"}, "203.0.113.9"},
		{"unparseable remote address", "not-an-address", nil, "not-an-address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := m.ClientIP(r); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// Rotating a spoofed X-Forwarded-For prefix must not reset the per-source
// counter, because the trailing element nginx appends does not change.
func TestLockoutNotBypassedByForwardedForRotation(t *testing.T) {
	m := NewManager(cheapConfig(t))
	attempt := func(spoof, pass string) error {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("X-Forwarded-For", spoof+", 198.51.100.7")
		return m.Login(httptest.NewRecorder(), req, "admin", pass)
	}

	for i := range maxFailures {
		if err := attempt(fmt.Sprintf("1.2.3.%d", i), "wrong"); err != ErrBadCredentials {
			t.Fatalf("attempt %d: err = %v, want ErrBadCredentials", i, err)
		}
	}
	if err := attempt("1.2.3.200", "wrong"); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited: the lockout was bypassed by rotating X-Forwarded-For", err)
	}
	// The correct password is still refused while the lockout holds.
	if err := attempt("1.2.3.201", testPassword); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// An attacker who really does have many source addresses hits the global
// backstop instead.
func TestGlobalFailureBackstop(t *testing.T) {
	m := NewManager(cheapConfig(t))
	attempt := func(i int, pass string) error {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1", i/256, i%256)
		return m.Login(httptest.NewRecorder(), req, "admin", pass)
	}

	for i := range globalMaxFailures {
		if err := attempt(i, "wrong"); err != ErrBadCredentials {
			t.Fatalf("attempt %d from a fresh address: err = %v, want ErrBadCredentials", i, err)
		}
	}
	if err := attempt(globalMaxFailures, "wrong"); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited after %d failures across distinct addresses",
			err, globalMaxFailures)
	}
	if err := attempt(globalMaxFailures+1, testPassword); err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	// It heals: a window in the past is discarded rather than latched.
	m.mu.Lock()
	m.global.first = time.Now().Add(-globalFailWindow - time.Minute)
	m.mu.Unlock()
	if err := attempt(globalMaxFailures+2, testPassword); err != nil {
		t.Fatalf("still locked out after the window rolled over: %v", err)
	}
}

// The failure map is keyed on a remote-supplied address, so it must not grow
// without bound between reaps.
func TestFailureMapIsBounded(t *testing.T) {
	m := NewManager(cheapConfig(t))
	for i := range maxFailureEntries * 2 {
		m.recordFailure(fmt.Sprintf("198.51.%d.%d", i/256, i%256))
	}
	m.mu.Lock()
	n := len(m.failures)
	m.mu.Unlock()
	if n > maxFailureEntries {
		t.Errorf("failure map holds %d entries, want at most %d", n, maxFailureEntries)
	}
}

// A login that cannot get a hashing slot must give up with the request rather
// than queue another 64 MiB allocation for a client that has gone away.
func TestLoginShedsWhenHashSlotsAreFullAndContextIsDone(t *testing.T) {
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	defer func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	}()

	m := NewManager(cheapConfig(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil).WithContext(ctx)
	req.RemoteAddr = "10.0.0.1:1"

	done := make(chan error, 1)
	go func() { done <- m.Login(httptest.NewRecorder(), req, "admin", testPassword) }()

	select {
	case err := <-done:
		if err != ErrRateLimited {
			t.Fatalf("Login = %v, want ErrRateLimited", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Login blocked on a full hashing slot instead of honouring the cancelled context")
	}

	if len(hashSlots) != cap(hashSlots) {
		t.Errorf("hashSlots holds %d of %d after shedding; a slot leaked", len(hashSlots), cap(hashSlots))
	}
}

// More concurrent logins than there are hashing slots must all complete.
func TestConcurrentLoginsDoNotDeadlock(t *testing.T) {
	m := NewManager(cheapConfig(t))
	const n = 32

	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			req.RemoteAddr = fmt.Sprintf("10.1.0.%d:1", i)
			<-start
			errs <- m.Login(httptest.NewRecorder(), req, "admin", testPassword)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Login: %v", err)
		}
	}
	if len(hashSlots) != 0 {
		t.Errorf("hashSlots holds %d after all logins returned; a slot leaked", len(hashSlots))
	}
}

// Rotating AUTH_PASSWORD_HASH must revoke cookies issued under the old one,
// and only those.
func TestPersistedSessionsAreBoundToTheCredential(t *testing.T) {
	store := NewSQLSessionStore(openDB(t))
	cfg := cheapConfig(t)

	first := NewManager(cfg)
	if err := first.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	cookie := login(t, first, "admin", testPassword)

	// Same hash string: a restart keeps the session.
	unchanged := NewManager(&config.Config{
		AuthUsername: cfg.AuthUsername, AuthPasswordHash: cfg.AuthPasswordHash, SessionTTLHours: 1})
	if err := unchanged.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if !unchanged.Authenticated(requestWith(cookie)) {
		t.Fatal("an unchanged password hash signed the user out")
	}

	// Re-hashing even the same password draws a fresh salt, so the string -
	// and the binding - changes.
	rotatedCfg := cheapConfig(t)
	if rotatedCfg.AuthPasswordHash == cfg.AuthPasswordHash {
		t.Fatal("re-hashing produced an identical string; the salt is not random")
	}
	rotated := NewManager(rotatedCfg)
	if err := rotated.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if rotated.Authenticated(requestWith(cookie)) {
		t.Error("a session issued under the old password hash survived the rotation")
	}
	live, err := store.Live(time.Now())
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("stale sessions remain in the store: %v", live)
	}

	// Sessions issued after the rotation survive the next restart as usual.
	fresh := login(t, rotated, "admin", testPassword)
	after := NewManager(rotatedCfg)
	if err := after.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if !after.Authenticated(requestWith(fresh)) {
		t.Error("a session issued under the current hash did not survive a restart")
	}
}

// A database that predates the binding has no recorded credential, so its
// sessions are not vouched for and must not be adopted.
func TestPersistDropsSessionsWithNoRecordedCredential(t *testing.T) {
	store := NewSQLSessionStore(openDB(t))
	if err := store.Save("legacy", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := NewManager(cheapConfig(t))
	if err := m.Persist(store); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if m.Authenticated(requestWith(&http.Cookie{Name: CookieName, Value: "legacy"})) {
		t.Error("an unbound session from an older database was adopted")
	}
}
