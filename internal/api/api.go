// Package api wires the HTTP surface: JSON endpoints, uploads, downloads and
// the server-sent-event stream.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"

	"aiofiles/internal/auth"
	"aiofiles/internal/config"
	"aiofiles/internal/jobs"
	"aiofiles/internal/proxy"
)

const (
	uploadsPath = "/api/uploads"
	healthPath  = "/api/health"
)

// Prober is a function rather than an interface so this package never imports
// internal/runner; the result is marshalled straight to JSON.
type Prober func(ctx context.Context, rawURL string) (any, error)

type Deps struct {
	Cfg   *config.Config
	Store *jobs.Store
	Queue *jobs.Queue
	Bus   *jobs.Bus
	Log   *slog.Logger
	Probe Prober

	// Auth is optional: sessions must live in the same Manager the handlers use,
	// so pass one in when the caller also runs StartReaper. When nil, New builds
	// its own and no reaper runs - expired sessions are still rejected lazily,
	// they just linger in the map.
	Auth *auth.Manager
}

type server struct {
	cfg        *config.Config
	store      *jobs.Store
	queue      *jobs.Queue
	bus        *jobs.Bus
	log        *slog.Logger
	probe      Prober
	probeLimit *probeLimiter
	auth       *auth.Manager
	trust      *proxy.Trust
}

func New(d Deps) http.Handler {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	am := d.Auth
	if am == nil {
		am = auth.NewManager(d.Cfg)
	}
	inFlight := 1
	var trusted []netip.Prefix
	if d.Cfg != nil {
		inFlight = d.Cfg.MaxConcurrentJobs
		trusted = d.Cfg.TrustedProxies
	}
	s := &server{
		cfg:        d.Cfg,
		store:      d.Store,
		queue:      d.Queue,
		bus:        d.Bus,
		log:        log,
		probe:      d.Probe,
		probeLimit: newProbeLimiter(inFlight),
		auth:       am,
		trust:      proxy.New(trusted),
	}
	return s.routes()
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+healthPath, s.handleHealth)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)

	// protect is a pass-through when auth is disabled.
	protect := s.auth.Middleware
	handle := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, protect(h))
	}

	handle("GET /api/presets", s.handlePresets)
	handle("POST /api/probe", s.handleProbe)
	handle("POST "+uploadsPath, s.handleUpload)
	handle("POST /api/jobs", s.handleCreateJob)
	handle("GET /api/jobs", s.handleListJobs)
	handle("GET /api/jobs/{id}", s.handleGetJob)
	handle("POST /api/jobs/{id}/cancel", s.handleCancelJob)
	handle("DELETE /api/jobs/{id}", s.handleDeleteJob)
	handle("GET /api/jobs/{id}/download", s.handleDownload)
	handle("GET /api/events", s.handleEvents)

	// Unknown API paths answer in JSON rather than net/http's plain text.
	// "/" is deliberately left free for whoever mounts the frontend.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	// Outermost first: a panic anywhere below still becomes a logged 500, and
	// a request to a name this instance does not answer to is turned away
	// before any handler sees it.
	return s.recoverMW(s.logMW(s.hostMW(s.originMW(s.limitBodyMW(mux)))))
}
