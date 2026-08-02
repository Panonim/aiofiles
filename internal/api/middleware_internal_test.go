package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recoverMW has to see the statusWriter logMW installs, or its "did the
// response already start?" check is handed the raw ResponseWriter, always
// answers no, and appends an error envelope to a reply that is already on the
// wire - a download or an SSE stream with `{"error":...}` glued to the end.
func TestPanicAfterTheResponseStartedDoesNotAppendABody(t *testing.T) {
	s := &server{log: slog.New(slog.DiscardHandler)}

	h := s.logMW(s.recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	})))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the response had already started", w.Code)
	}
	if body := w.Body.String(); body != "partial" {
		t.Errorf("body = %q, want %q", body, "partial")
	}
}

// The other half of the same contract: a panic before anything was written
// still has to produce the JSON 500.
func TestPanicBeforeTheResponseStartedIsA500(t *testing.T) {
	s := &server{log: slog.New(slog.DiscardHandler)}

	h := s.logMW(s.recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `"internal_error"`) {
		t.Errorf("body = %q, want an internal_error envelope", body)
	}
}
