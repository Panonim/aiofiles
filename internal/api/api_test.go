package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aiofiles/internal/api"
	"aiofiles/internal/auth"
	"aiofiles/internal/config"
	"aiofiles/internal/db"
	"aiofiles/internal/jobs"
)

const (
	testUser     = "admin"
	testPassword = "s3cret-password"
)

// argon2id at 64 MiB is deliberately slow, so hash the fixture once.
var hashOnce = sync.OnceValues(func() (string, error) { return auth.HashPassword(testPassword) })

// stubRunner lets Queue.Submit succeed without any media tool being installed.
// No workers are started, so submitted jobs simply stay queued.
type stubRunner struct{}

func (stubRunner) Run(context.Context, *jobs.Job, jobs.Emit) (jobs.Result, error) {
	return jobs.Result{}, nil
}

type testServer struct {
	*httptest.Server
	cfg    *config.Config
	store  *jobs.Store
	client *http.Client
}

func newTestServer(t *testing.T, withAuth bool) *testServer {
	t.Helper()

	base := t.TempDir()
	cfg := &config.Config{
		DataDir:              base,
		DBPath:               filepath.Join(base, "test.db"),
		DownloadDir:          filepath.Join(base, "downloads"),
		TmpDir:               filepath.Join(base, "tmp"),
		UploadDir:            filepath.Join(base, "uploads"),
		MaxUploadMiB:         1,
		MaxConcurrentJobs:    1,
		QueueDepth:           8,
		DefaultRetentionDays: 7,
		SessionTTLHours:      1,
	}
	for _, d := range []string{cfg.DownloadDir, cfg.TmpDir, cfg.UploadDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if withAuth {
		hash, err := hashOnce()
		if err != nil {
			t.Fatalf("hash password: %v", err)
		}
		cfg.AuthUsername, cfg.AuthPasswordHash = testUser, hash
	}

	handle, err := db.Open(t.Context(), cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	store := jobs.NewStore(handle)
	bus := jobs.NewBus()
	queue := jobs.NewQueue(store, bus, slog.New(slog.DiscardHandler), cfg.QueueDepth)
	for _, typ := range []jobs.Type{jobs.TypeDownload, jobs.TypeConvert, jobs.TypeCompress, jobs.TypeImage} {
		queue.Register(typ, stubRunner{})
	}

	handler := api.New(api.Deps{
		Cfg: cfg, Store: store, Queue: queue, Bus: bus,
		Log: slog.New(slog.DiscardHandler),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &testServer{Server: srv, cfg: cfg, store: store, client: &http.Client{Jar: jar}}
}

func (ts *testServer) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s %s: content type = %q, want JSON", method, path, ct)
	}
	return res.StatusCode, raw
}

// doWithHeaders is do() without its automatic Content-Type, for the tests that
// need to control the CSRF-relevant headers themselves.
func (ts *testServer) doWithHeaders(t *testing.T, method, path, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res.StatusCode, raw
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// errorField digs the field name out of a validation error envelope.
func errorField(t *testing.T, raw []byte) (code, field string) {
	t.Helper()
	body := decodeBody(t, raw)
	env, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response is not an error envelope: %s", raw)
	}
	code, _ = env["code"].(string)
	field, _ = env["field"].(string)
	return code, field
}

func TestHealthAndMeta(t *testing.T) {
	ts := newTestServer(t, false)

	t.Run("health", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodGet, "/api/health", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, raw)
		}
		body := decodeBody(t, raw)
		if body["status"] != "ok" || body["auth_required"] != false {
			t.Errorf("health = %s", raw)
		}
	})

	t.Run("me", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodGet, "/api/auth/me", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, raw)
		}
		body := decodeBody(t, raw)
		if body["authenticated"] != true || body["auth_required"] != false {
			t.Errorf("me = %s", raw)
		}
	})

	t.Run("presets", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodGet, "/api/presets", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, raw)
		}
		body := decodeBody(t, raw)
		if body["default_retention_days"] != float64(7) {
			t.Errorf("default_retention_days = %v", body["default_retention_days"])
		}
		if body["max_upload_bytes"] != float64(1<<20) {
			t.Errorf("max_upload_bytes = %v", body["max_upload_bytes"])
		}
		for _, key := range []string{"video_containers", "audio_formats", "image_formats", "crf_bounds"} {
			if _, ok := body[key]; !ok {
				t.Errorf("presets payload is missing %q", key)
			}
		}
	})
}

func TestAuthGate(t *testing.T) {
	ts := newTestServer(t, true)

	protected := []struct{ method, path string }{
		{http.MethodGet, "/api/jobs"},
		{http.MethodPost, "/api/jobs"},
		{http.MethodGet, "/api/jobs/abc"},
		{http.MethodDelete, "/api/jobs/abc"},
		{http.MethodGet, "/api/jobs/abc/download"},
		{http.MethodGet, "/api/presets"},
		{http.MethodPost, "/api/probe"},
		{http.MethodPost, "/api/uploads"},
		{http.MethodGet, "/api/events"},
	}
	for _, tc := range protected {
		t.Run("401 "+tc.method+" "+tc.path, func(t *testing.T) {
			status, raw := ts.do(t, tc.method, tc.path, "")
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", status, raw)
			}
			if code, _ := errorField(t, raw); code != "unauthorized" {
				t.Errorf("error code = %q, want unauthorized", code)
			}
		})
	}

	t.Run("health stays public", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodGet, "/api/health", "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, raw)
		}
		if decodeBody(t, raw)["auth_required"] != true {
			t.Errorf("health should advertise that auth is required: %s", raw)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodPost, "/api/auth/login",
			`{"username":"admin","password":"wrong"}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", status, raw)
		}
		if code, _ := errorField(t, raw); code != "invalid_credentials" {
			t.Errorf("error code = %q", code)
		}
	})

	t.Run("login then logout", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodPost, "/api/auth/login",
			`{"username":"`+testUser+`","password":"`+testPassword+`"}`)
		if status != http.StatusOK {
			t.Fatalf("login status = %d: %s", status, raw)
		}

		if status, raw := ts.do(t, http.MethodGet, "/api/jobs", ""); status != http.StatusOK {
			t.Fatalf("authenticated GET /api/jobs = %d: %s", status, raw)
		}
		status, raw = ts.do(t, http.MethodGet, "/api/auth/me", "")
		body := decodeBody(t, raw)
		if status != http.StatusOK || body["authenticated"] != true || body["username"] != testUser {
			t.Fatalf("me = %d %s", status, raw)
		}

		if status, raw := ts.do(t, http.MethodPost, "/api/auth/logout", ""); status != http.StatusOK {
			t.Fatalf("logout = %d: %s", status, raw)
		}
		if status, _ := ts.do(t, http.MethodGet, "/api/jobs", ""); status != http.StatusUnauthorized {
			t.Fatalf("GET /api/jobs after logout = %d, want 401", status)
		}
	})
}

func TestCreateJobRejectsBadRequests(t *testing.T) {
	ts := newTestServer(t, false)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"not json", `{`, http.StatusBadRequest, "invalid_json", ""},
		{"empty object", `{}`, http.StatusBadRequest, "invalid_params", "type"},
		{"unknown type", `{"type":"frobnicate"}`, http.StatusBadRequest, "invalid_params", "type"},
		{"type is not a string", `{"type":7}`, http.StatusBadRequest, "invalid_json", ""},
		{"download without url", `{"type":"download"}`, http.StatusBadRequest, "invalid_params", "url"},
		{"download with file url", `{"type":"download","url":"file:///etc/passwd"}`,
			http.StatusBadRequest, "invalid_params", "url"},
		{"download with a dash url", `{"type":"download","url":"-x https://example.com"}`,
			http.StatusBadRequest, "invalid_params", "url"},
		{"format selector syntax", `{"type":"download","url":"https://example.com/v","params":{"format_id":"best[height<=720]"}}`,
			http.StatusBadRequest, "invalid_params", "format_id"},
		{"unknown param", `{"type":"download","url":"https://example.com/v","params":{"proxy":"http://evil"}}`,
			http.StatusBadRequest, "invalid_params", "params"},
		{"retention off the menu", `{"type":"download","url":"https://example.com/v","params":{"retention_days":3}}`,
			http.StatusBadRequest, "invalid_params", "retention_days"},
		{"convert without upload", `{"type":"convert"}`, http.StatusBadRequest, "invalid_params", "upload_id"},
		{"convert with traversal upload id", `{"type":"convert","upload_id":"../../etc/passwd"}`,
			http.StatusBadRequest, "invalid_params", "upload_id"},
		{"convert with absolute upload id", `{"type":"convert","upload_id":"/etc/passwd"}`,
			http.StatusBadRequest, "invalid_params", "upload_id"},
		{"convert with unknown upload id", `{"type":"convert","upload_id":"deadbeefdeadbeef-clip.mp4"}`,
			http.StatusBadRequest, "invalid_params", "upload_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := ts.do(t, http.MethodPost, "/api/jobs", tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", status, tc.wantStatus, raw)
			}
			code, field := errorField(t, raw)
			if code != tc.wantCode {
				t.Errorf("error code = %q, want %q", code, tc.wantCode)
			}
			if field != tc.wantField {
				t.Errorf("error field = %q, want %q", field, tc.wantField)
			}
		})
	}

	if list, err := ts.store.List(t.Context(), "", 0); err != nil || len(list) != 0 {
		t.Errorf("a rejected request created a job: %v (%v)", list, err)
	}
}

func TestCreateDownloadJob(t *testing.T) {
	ts := newTestServer(t, false)

	status, raw := ts.do(t, http.MethodPost, "/api/jobs",
		`{"type":"download","url":"https://example.com/watch?v=abc","params":{"mode":"audio","audio_format":"mp3"}}`)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", status, raw)
	}

	body := decodeBody(t, raw)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no job id in %s", raw)
	}
	if body["status"] != "queued" || body["type"] != "download" {
		t.Errorf("job = %s", raw)
	}
	if body["expires_at"] == nil {
		t.Error("a job with a retention window should carry expires_at")
	}

	stored, err := ts.store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("job was not persisted: %v", err)
	}
	// The client's raw params never reach the database.
	var params map[string]any
	if err := json.Unmarshal(stored.Params, &params); err != nil {
		t.Fatalf("stored params are not JSON: %v", err)
	}
	if params["mode"] != "audio" || params["audio_format"] != "mp3" {
		t.Errorf("stored params = %s", stored.Params)
	}
	if _, ok := params["retention_days"]; !ok {
		t.Error("stored params lost the validated retention")
	}

	if status, raw := ts.do(t, http.MethodGet, "/api/jobs/"+id, ""); status != http.StatusOK {
		t.Fatalf("GET /api/jobs/%s = %d: %s", id, status, raw)
	}
}

// The UI iterates the list unconditionally; a null would break it.
func TestJobsListEncodesEmptyArray(t *testing.T) {
	ts := newTestServer(t, false)

	status, raw := ts.do(t, http.MethodGet, "/api/jobs", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if !strings.Contains(string(raw), `"jobs":[]`) {
		t.Fatalf("empty list encoded as %s, want an empty array", raw)
	}

	if status, raw := ts.do(t, http.MethodPost, "/api/jobs",
		`{"type":"download","url":"https://example.com/v"}`); status != http.StatusAccepted {
		t.Fatalf("create = %d: %s", status, raw)
	}

	status, raw = ts.do(t, http.MethodGet, "/api/jobs", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	list, ok := decodeBody(t, raw)["jobs"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("jobs = %s, want one entry", raw)
	}

	t.Run("filtered list is also an array", func(t *testing.T) {
		_, raw := ts.do(t, http.MethodGet, "/api/jobs?status=done", "")
		if !strings.Contains(string(raw), `"jobs":[]`) {
			t.Errorf("filtered list = %s", raw)
		}
	})

	t.Run("bad limit", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodGet, "/api/jobs?limit=abc", "")
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", status, raw)
		}
		if _, field := errorField(t, raw); field != "limit" {
			t.Errorf("field = %q, want limit", field)
		}
	})
}

func TestUploadThenConvert(t *testing.T) {
	ts := newTestServer(t, false)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "my holiday video.mp4")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write([]byte("not really a video")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/uploads", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d: %s", res.StatusCode, raw)
	}

	body := decodeBody(t, raw)
	uploadID, _ := body["upload_id"].(string)
	if uploadID == "" {
		t.Fatalf("no upload id in %s", raw)
	}
	if body["filename"] != "my_holiday_video.mp4" {
		t.Errorf("filename = %v, want the sanitised name", body["filename"])
	}
	if body["size"] != float64(len("not really a video")) {
		t.Errorf("size = %v", body["size"])
	}
	if _, err := os.Stat(filepath.Join(ts.cfg.UploadDir, uploadID)); err != nil {
		t.Errorf("upload was not stored: %v", err)
	}

	status, raw := ts.do(t, http.MethodPost, "/api/jobs",
		`{"type":"convert","upload_id":"`+uploadID+`","params":{"container":"mkv","video_codec":"h265","crf":24}}`)
	if status != http.StatusAccepted {
		t.Fatalf("create convert job = %d: %s", status, raw)
	}
	if src, _ := decodeBody(t, raw)["source"].(string); src != "my_holiday_video.mp4" {
		t.Errorf("job source = %q, want the original filename without the token", src)
	}

	t.Run("missing file field", func(t *testing.T) {
		var empty bytes.Buffer
		w := multipart.NewWriter(&empty)
		if err := w.WriteField("notafile", "x"); err != nil {
			t.Fatalf("write field: %v", err)
		}
		w.Close()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/uploads", &empty)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		res, err := ts.client.Do(req)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("not multipart", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodPost, "/api/uploads", `{"file":"x"}`)
		if status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400: %s", status, raw)
		}
	})
}

func TestJobLifecycleEndpoints(t *testing.T) {
	ts := newTestServer(t, false)

	status, raw := ts.do(t, http.MethodPost, "/api/jobs",
		`{"type":"download","url":"https://example.com/v"}`)
	if status != http.StatusAccepted {
		t.Fatalf("create = %d: %s", status, raw)
	}
	id, _ := decodeBody(t, raw)["id"].(string)

	t.Run("download before the job is done", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodGet, "/api/jobs/"+id+"/download", "")
		if status != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", status, raw)
		}
		if code, _ := errorField(t, raw); code != "not_ready" {
			t.Errorf("code = %q, want not_ready", code)
		}
	})

	t.Run("cancel a job that is not running", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodPost, "/api/jobs/"+id+"/cancel", "")
		if status != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("delete", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodDelete, "/api/jobs/"+id, "")
		if status != http.StatusOK {
			t.Fatalf("status = %d: %s", status, raw)
		}
		if status, _ := ts.do(t, http.MethodGet, "/api/jobs/"+id, ""); status != http.StatusNotFound {
			t.Errorf("GET after delete = %d, want 404", status)
		}
	})
}

func TestNotFoundIsJSON(t *testing.T) {
	ts := newTestServer(t, false)

	tests := []struct{ name, method, path string }{
		{"unknown job", http.MethodGet, "/api/jobs/does-not-exist"},
		{"unknown job download", http.MethodGet, "/api/jobs/does-not-exist/download"},
		{"unknown endpoint", http.MethodGet, "/api/nope"},
		{"unknown nested endpoint", http.MethodPost, "/api/jobs/x/y/z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := ts.do(t, tc.method, tc.path, "")
			if status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", status, raw)
			}
			if code, _ := errorField(t, raw); code != "not_found" {
				t.Errorf("code = %q, want not_found", code)
			}
		})
	}
}

func TestProbeWithoutABackend(t *testing.T) {
	ts := newTestServer(t, false)

	t.Run("invalid url is rejected before the backend", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodPost, "/api/probe", `{"url":"file:///etc/passwd"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", status, raw)
		}
	})

	t.Run("no prober configured", func(t *testing.T) {
		status, raw := ts.do(t, http.MethodPost, "/api/probe", `{"url":"https://example.com/v"}`)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", status, raw)
		}
	})
}

// With auth disabled there is no cookie to withhold, so the Origin check and
// the JSON Content-Type check are the only things standing between a random
// web page and this instance.
func TestCrossOriginRequestsAreRejected(t *testing.T) {
	ts := newTestServer(t, false)
	const job = `{"type":"download","url":"https://example.com/v"}`
	const ctJSON = "application/json"

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		hdr        map[string]string
		wantStatus int
		wantCode   string
	}{
		{"cross-origin post", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON, "Origin": "http://evil.example"},
			http.StatusForbidden, "cross_origin"},
		{"cross-origin post to probe", http.MethodPost, "/api/probe", `{"url":"https://example.com/v"}`,
			map[string]string{"Content-Type": ctJSON, "Origin": "https://attacker.test"},
			http.StatusForbidden, "cross_origin"},
		{"cross-origin delete", http.MethodDelete, "/api/jobs/whatever", "",
			map[string]string{"Origin": "http://evil.example"},
			http.StatusForbidden, "cross_origin"},
		{"opaque origin", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON, "Origin": "null"},
			http.StatusForbidden, "cross_origin"},
		{"scheme mismatch", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON, "Origin": "https://" + hostOf(t, ts.URL)},
			http.StatusForbidden, "cross_origin"},
		{"same-origin post", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON, "Origin": ts.URL},
			http.StatusAccepted, ""},
		{"absent origin", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON},
			http.StatusAccepted, ""},
		{"sec-fetch-site same-origin", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON, "Sec-Fetch-Site": "same-origin"},
			http.StatusAccepted, ""},
		{"sec-fetch-site cross-site with a foreign origin", http.MethodPost, "/api/jobs", job,
			map[string]string{"Content-Type": ctJSON, "Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"},
			http.StatusForbidden, "cross_origin"},
		// The bundled nginx forwards $host, which has no port, so the port in the
		// browser's Origin must not defeat the comparison.
		{"forwarded host from the proxy", http.MethodPost, "/api/jobs", job,
			map[string]string{
				"Content-Type": ctJSON, "Origin": "https://media.example.com:8443",
				"X-Forwarded-Host": "media.example.com", "X-Forwarded-Proto": "https",
			},
			http.StatusAccepted, ""},
		{"forwarded host does not match", http.MethodPost, "/api/jobs", job,
			map[string]string{
				"Content-Type": ctJSON, "Origin": "https://evil.example",
				"X-Forwarded-Host": "media.example.com", "X-Forwarded-Proto": "https",
			},
			http.StatusForbidden, "cross_origin"},
		// Reads are not state-changing and the attacker cannot see the response.
		{"get with a foreign origin", http.MethodGet, "/api/jobs", "",
			map[string]string{"Origin": "http://evil.example"},
			http.StatusOK, ""},
		{"health with a foreign origin", http.MethodGet, "/api/health", "",
			map[string]string{"Origin": "http://evil.example"},
			http.StatusOK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := ts.doWithHeaders(t, tc.method, tc.path, tc.body, tc.hdr)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", status, tc.wantStatus, raw)
			}
			if tc.wantCode != "" {
				if code, _ := errorField(t, raw); code != tc.wantCode {
					t.Errorf("error code = %q, want %q", code, tc.wantCode)
				}
			}
		})
	}
}

// The enctype="text/plain" form trick only works if nobody checks Content-Type.
func TestJSONContentTypeIsRequired(t *testing.T) {
	ts := newTestServer(t, false)
	const job = `{"type":"download","url":"https://example.com/v"}`

	tests := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{"text/plain", "text/plain", http.StatusUnsupportedMediaType},
		{"form urlencoded", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"multipart", "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
		{"absent", "", http.StatusUnsupportedMediaType},
		{"unparsable", "application/json;;", http.StatusUnsupportedMediaType},
		{"json", "application/json", http.StatusAccepted},
		{"json with a charset", "application/json; charset=utf-8", http.StatusAccepted},
		{"json with odd case", "Application/JSON", http.StatusAccepted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.contentType != "" {
				hdr["Content-Type"] = tc.contentType
			}
			status, raw := ts.doWithHeaders(t, http.MethodPost, "/api/jobs", job, hdr)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", status, tc.wantStatus, raw)
			}
			if tc.wantStatus == http.StatusUnsupportedMediaType {
				if code, _ := errorField(t, raw); code != "unsupported_media_type" {
					t.Errorf("error code = %q, want unsupported_media_type", code)
				}
			}
		})
	}

	// Endpoints that take no body must not start demanding a Content-Type.
	t.Run("bodyless post is unaffected", func(t *testing.T) {
		status, raw := ts.doWithHeaders(t, http.MethodPost, "/api/jobs/nope/cancel", "", nil)
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", status, raw)
		}
	})
}

// The upload endpoint is multipart, so the JSON rule must not reach it - but
// the Origin check still must.
func TestUploadOriginHandling(t *testing.T) {
	ts := newTestServer(t, false)

	upload := func(t *testing.T, origin string) (int, []byte) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("file", "clip.mp4")
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write([]byte("bytes")); err != nil {
			t.Fatalf("write part: %v", err)
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/uploads", &buf)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := ts.client.Do(req)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, raw
	}

	t.Run("same-origin upload still works", func(t *testing.T) {
		status, raw := upload(t, ts.URL)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, raw)
		}
		if id, _ := decodeBody(t, raw)["upload_id"].(string); id == "" {
			t.Errorf("no upload id in %s", raw)
		}
	})

	t.Run("upload without an origin still works", func(t *testing.T) {
		if status, raw := upload(t, ""); status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, raw)
		}
	})

	t.Run("cross-origin upload is rejected", func(t *testing.T) {
		status, raw := upload(t, "http://evil.example")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", status, raw)
		}
		if code, _ := errorField(t, raw); code != "cross_origin" {
			t.Errorf("code = %q, want cross_origin", code)
		}
	})
}

// hostOf returns the host:port of a test server URL.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u.Host
}

func TestOversizedJSONBodyIsRejected(t *testing.T) {
	ts := newTestServer(t, false)

	body := `{"type":"download","url":"https://example.com/v","params":{"format_id":"` +
		strings.Repeat("a", 1<<20) + `"}}`
	status, raw := ts.do(t, http.MethodPost, "/api/jobs", body)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", status, raw)
	}
	if code, _ := errorField(t, raw); code != "body_too_large" {
		t.Errorf("code = %q, want body_too_large", code)
	}
}

// guardHandler is the middleware chain on its own: the host guard runs before
// any handler, so these tests only ever reach endpoints that need no database.
func guardHandler(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	return api.New(api.Deps{Cfg: cfg, Log: slog.New(slog.DiscardHandler)})
}

func guardRequest(t *testing.T, h http.Handler, remoteAddr, host string, hdr map[string]string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/presets", nil)
	r.RemoteAddr = remoteAddr
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// PROXY_ONLY exists so that reaching the box by address stops working while the
// name the proxy answers to keeps working.
func TestProxyOnlyBlocksAddressAccess(t *testing.T) {
	h := guardHandler(t, &config.Config{ProxyOnly: true})

	tests := []struct {
		name       string
		remoteAddr string
		host       string
		hdr        map[string]string
		want       int
	}{
		{"ip and port", "127.0.0.1:1", "192.168.16.35:19882", nil, http.StatusForbidden},
		{"bare ip", "127.0.0.1:1", "192.168.16.35", nil, http.StatusForbidden},
		{"ipv6 literal", "127.0.0.1:1", "[fd00::1]:19882", nil, http.StatusForbidden},
		{"loopback by address", "127.0.0.1:1", "127.0.0.1:1144", nil, http.StatusForbidden},
		{"no host at all", "127.0.0.1:1", "", nil, http.StatusForbidden},
		{"hostname through the proxy", "127.0.0.1:1", "aio.example.com", nil, http.StatusOK},
		// The bundled nginx forwards $host; an address there is still an address.
		{"forwarded hostname", "127.0.0.1:1", "127.0.0.1:1144",
			map[string]string{"X-Forwarded-Host": "aio.example.com"}, http.StatusOK},
		{"forwarded address", "127.0.0.1:1", "aio.example.com",
			map[string]string{"X-Forwarded-Host": "192.168.16.35"}, http.StatusForbidden},
		// Running the binary with no proxy in front and PROXY_ONLY set is a
		// misconfiguration, and it fails closed.
		{"untrusted peer", "192.168.16.20:5000", "aio.example.com", nil, http.StatusForbidden},
		// A client cannot talk its way past the guard with a header.
		{"untrusted peer claiming a good host", "192.168.16.20:5000", "192.168.16.35:19882",
			map[string]string{"X-Forwarded-Host": "aio.example.com"}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := guardRequest(t, h, tc.remoteAddr, tc.host, tc.hdr); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAllowedHostsGuard(t *testing.T) {
	cfg := &config.Config{AllowedHosts: []string{"aio.example.com", "*.media.example.com"}}
	h := guardHandler(t, cfg)

	tests := []struct {
		host string
		want int
	}{
		{"aio.example.com", http.StatusOK},
		{"AIO.Example.com:8443", http.StatusOK},
		{"files.media.example.com", http.StatusOK},
		{"media.example.com", http.StatusForbidden},
		{"192.168.16.35:19882", http.StatusForbidden},
		{"aio.example.com.evil.net", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := guardRequest(t, h, "127.0.0.1:1", tc.host, nil); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// The container healthcheck reaches the app over loopback with an IP in Host,
// so locking it out would only make a correctly configured container look
// unhealthy.
func TestHealthSurvivesTheHostGuard(t *testing.T) {
	h := guardHandler(t, &config.Config{ProxyOnly: true, AllowedHosts: []string{"aio.example.com"}})

	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "127.0.0.1:8000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the healthcheck is locked out", w.Code)
	}
}

// Nothing set is the shipped default and must not start refusing LAN access.
func TestHostGuardIsOffByDefault(t *testing.T) {
	h := guardHandler(t, &config.Config{})
	for _, host := range []string{"192.168.16.35:19882", "aio.example.com", ""} {
		if got := guardRequest(t, h, "192.168.16.20:5000", host, nil); got != http.StatusOK {
			t.Errorf("host %q: status = %d, want 200", host, got)
		}
	}
}

// With TRUSTED_PROXIES set, the app has to believe the hop in front of nginx
// rather than nginx itself.
func TestTrustedProxyIsBelievedForTheOrigin(t *testing.T) {
	cfg := &config.Config{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		ProxyOnly:      true,
	}
	h := guardHandler(t, cfg)

	// Reached directly by an operator's proxy, with no bundled nginx in the way.
	if got := guardRequest(t, h, "10.0.0.5:5000", "aio.example.com", nil); got != http.StatusOK {
		t.Errorf("status = %d, want 200 for a trusted proxy", got)
	}
	if got := guardRequest(t, h, "10.1.2.3:5000", "192.168.16.35", nil); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an address even from a trusted proxy", got)
	}
}
