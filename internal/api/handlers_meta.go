package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"aiofiles/internal/auth"
	"aiofiles/internal/presets"
)

// yt-dlp occasionally hangs on a slow extractor; the browser should not wait forever.
const probeTimeout = 30 * time.Second

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"auth_required": s.cfg.AuthEnabled(),
	})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	switch err := s.auth.Login(w, r, req.Username, req.Password); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
	case errors.Is(err, auth.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "too_many_attempts",
			"too many failed login attempts, try again later")
	default:
		s.log.Error("login failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "login failed")
	}
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	authed := s.auth.Authenticated(r)
	username := ""
	if authed {
		username = s.cfg.AuthUsername
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": authed,
		"username":      username,
		"auth_required": s.cfg.AuthEnabled(),
	})
}

func (s *server) handlePresets(w http.ResponseWriter, r *http.Request) {
	out := presets.All()
	out["default_retention_days"] = s.cfg.DefaultRetentionDays
	out["max_upload_bytes"] = s.cfg.MaxUploadBytes()
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	clean, err := presets.ValidateSourceURL(req.URL)
	if err != nil {
		writeValidationError(w, err)
		return
	}
	if s.probe == nil {
		writeError(w, http.StatusServiceUnavailable, "probe_unavailable", "metadata lookup is not configured")
		return
	}

	release, ok := s.probeLimit.acquire()
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "probe_busy",
			"too many metadata lookups in flight, try again shortly")
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	info, err := s.probe(ctx, clean)
	if err != nil {
		// Our deadline, not the client hanging up.
		if ctx.Err() != nil && r.Context().Err() == nil {
			writeError(w, http.StatusGatewayTimeout, "probe_timeout", "metadata lookup timed out")
			return
		}
		// yt-dlp quotes fragments of what it fetched, which would make this an
		// oracle for whatever the extractor reached.
		s.log.Warn("probe failed", "url", clean, "err", err)
		writeError(w, http.StatusBadGateway, "probe_failed",
			"could not read metadata for that URL")
		return
	}
	writeJSON(w, http.StatusOK, info)
}
