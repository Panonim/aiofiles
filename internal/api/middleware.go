package api

import (
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// statusWriter records the response code for the debug log and lets recoverMW
// know whether headers already went out. Unwrap keeps http.NewResponseController
// (SSE flushing) working through the chain.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
	bytes   int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.status = code
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recoverMW turns a panic into a logged 500 instead of a dropped connection.
func (s *server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec) // the server's own "drop this connection" signal
			}
			s.log.Error("panic in handler",
				"method", r.Method, "path", r.URL.Path,
				"panic", rec, "stack", string(debug.Stack()))

			if sw, ok := w.(*statusWriter); ok && sw.written {
				return // response already started; nothing sane left to send
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// One line per request, at Debug only: this box is tuned for a quiet log.
func (s *server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		s.log.Debug("http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "dur", time.Since(start).Round(time.Millisecond))
	})
}

// hostMW enforces the two reverse-proxy settings: ALLOWED_HOSTS, the names this
// instance answers to, and PROXY_ONLY, which additionally refuses anything that
// did not arrive through a proxy - the point being that
// http://192.168.1.10:1144 stops working while https://aio.example.com keeps
// working, so the proxy's TLS and access control cannot be walked around by
// anyone who can reach the port.
//
// The bundled nginx applies the same host rules to the static frontend, so a
// blocked name does not get as far as a loading UI that then fails on every
// call. This is the backstop for a deployment that puts something else in front
// or runs the binary on its own.
func (s *server) hostMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg == nil || (!s.cfg.ProxyOnly && len(s.cfg.AllowedHosts) == 0) {
			next.ServeHTTP(w, r)
			return
		}
		// The container healthcheck asks for this over loopback with an IP in
		// Host. Locking it out would only make a correctly configured container
		// report itself unhealthy, and the reply is a status and a boolean.
		if r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}

		host := s.trust.Hostname(r)
		if s.cfg.ProxyOnly {
			// Without a proxy in front there is no forwarding hop to trust, so
			// the peer is the client itself. In the shipped image the peer is
			// always the bundled nginx and this is satisfied by construction.
			if !s.trust.FromProxy(r) {
				s.reject(w, r, host, "direct connection from an untrusted address")
				return
			}
			// The name is what separates "came through the proxy" from "typed
			// the box's address into a browser": a proxy is reached by the name
			// it holds a certificate for, an IP literal never is.
			if host == "" || isIPLiteral(host) {
				s.reject(w, r, host, "PROXY_ONLY is set and the request used an address rather than a hostname")
				return
			}
		}
		if !s.cfg.HostAllowed(host) {
			s.reject(w, r, host, "host is not in ALLOWED_HOSTS")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) reject(w http.ResponseWriter, r *http.Request, host, why string) {
	s.log.Warn("rejected request", "host", host, "path", r.URL.Path,
		"peer", r.RemoteAddr, "reason", why)
	writeError(w, http.StatusForbidden, "host_not_allowed",
		"this instance does not answer to that address")
}

// A Host of "192.168.16.35" or "[fd00::1]" is an address, not a name. The port
// is already stripped by the time this is called.
func isIPLiteral(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	_, err := netip.ParseAddr(host)
	return err == nil
}

// originMW is the whole cross-origin defence: a state-changing request may not
// carry an Origin belonging to another site. With auth disabled - the shipped
// default - there is no cookie to withhold, so this is what stops a random web
// page from driving the instance.
func (s *server) originMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.sameOrigin(r) {
				s.log.Warn("rejected cross-origin request",
					"method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"))
				writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) sameOrigin(r *http.Request) bool {
	// The browser computes this against the real request URL and page script
	// cannot forge it, so same-origin/none is an authoritative allow signal.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	}

	origin := r.Header.Get("Origin")
	// No Origin means no browser page behind the request: curl and the scripted
	// flows in docs/api.md must keep working.
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // "null" and other opaque origins are not this site
	}
	// Only scheme and hostname are compared: the bundled nginx forwards $host,
	// which has no port, and it listens on a different port than the one
	// published to the browser, so the port is not knowable here.
	scheme, host := s.trust.Origin(r)
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Hostname(), host)
}

// Uploads carry their own, much larger limit and are skipped here.
func (s *server) limitBodyMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.URL.Path != uploadsPath {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		}
		next.ServeHTTP(w, r)
	})
}
