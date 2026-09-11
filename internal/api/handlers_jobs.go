package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aiofiles/internal/jobs"
	"aiofiles/internal/presets"
)

type createJobRequest struct {
	Type     string          `json:"type"`
	URL      string          `json:"url"`
	UploadID string          `json:"upload_id"`
	Params   json.RawMessage `json:"params"`
}

func (s *server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	typ := jobs.Type(strings.TrimSpace(req.Type))
	if !typ.Valid() {
		writeValidationError(w, &presets.ValidationError{
			Field: "type", Reason: "must be download, convert, compress, image or edit",
		})
		return
	}

	job := &jobs.Job{Type: typ}

	if typ == jobs.TypeDownload {
		clean, err := presets.ValidateSourceURL(req.URL)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		job.Source = clean
		job.Title = clean
	} else {
		if strings.TrimSpace(req.UploadID) == "" {
			writeValidationError(w, &presets.ValidationError{
				Field: "upload_id", Reason: "is required for this job type",
			})
			return
		}
		path, err := s.resolveUpload(req.UploadID)
		if err != nil {
			writeValidationError(w, &presets.ValidationError{
				Field: "upload_id", Reason: "unknown or invalid upload",
			})
			return
		}
		job.InputPath = path
		job.Source = originalName(filepath.Base(path))
		job.Title = job.Source
	}

	// Re-marshal the validated struct: the client's raw JSON never reaches the
	// database or a runner.
	validated, retentionDays, err := parseParams(typ, req.Params, s.cfg.DefaultRetentionDays)
	if err != nil {
		writeValidationError(w, err)
		return
	}
	encoded, err := json.Marshal(validated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not encode parameters")
		return
	}
	job.Params = encoded

	if retentionDays > 0 {
		exp := time.Now().UTC().Add(time.Duration(retentionDays) * 24 * time.Hour)
		job.ExpiresAt = &exp
	}

	if err := s.queue.Submit(r.Context(), job); err != nil {
		switch {
		case errors.Is(err, jobs.ErrQueueFull):
			writeError(w, http.StatusServiceUnavailable, "queue_full", "the job queue is full, try again shortly")
		case errors.Is(err, jobs.ErrNoRunner):
			writeError(w, http.StatusServiceUnavailable, "no_runner", "no runner is registered for this job type")
		default:
			s.log.Error("submit job", "type", typ, "err", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "could not queue the job")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// Returns the validated payload plus its retention in days (0 means keep forever).
func parseParams(typ jobs.Type, raw json.RawMessage, defaultRetention int) (any, int, error) {
	switch typ {
	case jobs.TypeDownload:
		p, err := presets.ParseDownload(raw, defaultRetention)
		if err != nil {
			return nil, 0, err
		}
		return p, p.RetentionDays, nil
	case jobs.TypeConvert:
		p, err := presets.ParseConvert(raw, defaultRetention)
		if err != nil {
			return nil, 0, err
		}
		return p, p.RetentionDays, nil
	case jobs.TypeCompress:
		p, err := presets.ParseCompress(raw, defaultRetention)
		if err != nil {
			return nil, 0, err
		}
		return p, p.RetentionDays, nil
	case jobs.TypeImage:
		p, err := presets.ParseImage(raw, defaultRetention)
		if err != nil {
			return nil, 0, err
		}
		return p, p.RetentionDays, nil
	case jobs.TypeEdit:
		p, err := presets.ParseEdit(raw, defaultRetention)
		if err != nil {
			return nil, 0, err
		}
		return p, p.RetentionDays, nil
	}
	return nil, 0, &presets.ValidationError{Field: "type", Reason: "unsupported job type"}
}

func (s *server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	status := jobs.Status(strings.TrimSpace(r.URL.Query().Get("status")))
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeValidationError(w, &presets.ValidationError{Field: "limit", Reason: "must be a non-negative integer"})
			return
		}
		limit = n
	}

	list, err := s.store.List(r.Context(), status, limit)
	if err != nil {
		s.log.Error("list jobs", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list jobs")
		return
	}
	if list == nil {
		list = []*jobs.Job{} // encode as [], never null
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": list})
}

func (s *server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.queue.Cancel(job.ID); err != nil {
		writeError(w, http.StatusConflict, "not_cancelable", "job is not running")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "id": job.ID})
}

func (s *server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.lookup(w, r)
	if !ok {
		return
	}

	// Stop the child first, or it keeps writing to a file nobody owns any more.
	_ = s.queue.Cancel(job.ID)

	s.removeFile(job.OutputPath)
	s.removeFile(job.InputPath)

	if err := s.store.Delete(r.Context(), job.ID); err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		s.log.Error("delete job", "id", job.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not delete the job")
		return
	}
	s.bus.Publish(jobs.Event{Kind: jobs.EventDeleted, JobID: job.ID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": job.ID})
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	job, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if job.Status != jobs.StatusDone {
		writeError(w, http.StatusConflict, "not_ready",
			fmt.Sprintf("job status is %q, not done", job.Status))
		return
	}
	if job.OutputPath == "" {
		writeError(w, http.StatusNotFound, "no_output", "this job produced no file")
		return
	}
	st, err := os.Stat(job.OutputPath)
	if err != nil || !st.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "file_missing", "the output file is no longer on disk")
		return
	}

	name := filepath.Base(job.OutputPath)
	// FormatMediaType emits the RFC 5987 filename* form for non-ASCII names.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))

	if s.cfg.XAccelPrefix != "" {
		if internal, ok := accelPath(s.cfg.XAccelPrefix, s.cfg.DownloadDir, job.OutputPath); ok {
			w.Header().Set("X-Accel-Redirect", internal)
			w.Header().Set("Content-Type", contentTypeFor(name))
			w.WriteHeader(http.StatusOK) // nginx replaces the (empty) body
			return
		}
		s.log.Debug("output outside download dir, streaming through Go", "path", job.OutputPath)
	}
	http.ServeFile(w, r, job.OutputPath)
}

// accelPath builds the internal nginx location for an output file. Reports
// false when the file is not under root, in which case Go must serve it.
func accelPath(prefix, root, outputPath string) (string, bool) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absRoot, absOut)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}

	segments := strings.Split(filepath.ToSlash(rel), "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.Join(segments, "/"), true
}

func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// lookup loads the {id} path value, writing the 404/500 itself when it fails.
func (s *server) lookup(w http.ResponseWriter, r *http.Request) (*jobs.Job, bool) {
	id := r.PathValue("id")
	job, err := s.store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "job not found")
			return nil, false
		}
		s.log.Error("load job", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load the job")
		return nil, false
	}
	return job, true
}

func (s *server) removeFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.log.Warn("remove job file", "path", path, "err", err)
	}
}
