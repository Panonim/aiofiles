package api

import (
	"net/http"
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

// originMW is the whole cross-origin defence: a state-changing request may not
// carry an Origin belonging to another site. With auth disabled - the shipped
// default - there is no cookie to withhold, so this is what stops a random web
// page from driving the instance.
func (s *server) originMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOrigin(r) {
				s.log.Warn("rejected cross-origin request",
					"method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"))
				writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
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
	scheme, host := requestOrigin(r)
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Hostname(), host)
}

// requestOrigin reconstructs this server's own origin. The bundled nginx sets
// Host/X-Forwarded-Host from $host, which drops the port, and it listens on a
// different port than the one published to the browser - so the port is simply
// not knowable here and only scheme+hostname are compared.
func requestOrigin(r *http.Request) (scheme, host string) {
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := firstValue(r.Header.Get("X-Forwarded-Proto")); v != "" {
		scheme = v
	}
	host = r.Host
	if v := firstValue(r.Header.Get("X-Forwarded-Host")); v != "" {
		host = v
	}
	return scheme, (&url.URL{Host: host}).Hostname()
}

// A chain of proxies appends to these headers; the first entry is the client's.
func firstValue(header string) string {
	first, _, _ := strings.Cut(header, ",")
	return strings.TrimSpace(first)
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
