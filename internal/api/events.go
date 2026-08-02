package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Must stay below the idle timeout of any proxy in front of us; 25s is
// comfortably under nginx's 60s default.
const keepaliveInterval = 25 * time.Second

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // stop nginx buffering the stream
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// The stream is long-lived by definition; clear any per-write deadline the
	// server may impose. Not all writers support it, so the error is ignored.
	_ = rc.SetWriteDeadline(time.Time{})

	// send reports false once the client is gone, which ends the stream.
	send := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	events, unsubscribe := s.bus.Subscribe()
	defer unsubscribe()

	if !send("event: ready\ndata: {}\n\n") {
		return
	}

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				s.log.Debug("encode sse event", "err", err)
				continue
			}
			if !send("event: %s\ndata: %s\n\n", ev.Kind, payload) {
				return
			}

		case <-ticker.C:
			if !send(": keepalive\n\n") {
				return
			}
		}
	}
}
