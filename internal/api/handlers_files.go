package api

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"aiofiles/internal/jobs"
)

type reusableFile struct {
	JobID     string    `json:"job_id"`
	Source    string    `json:"source"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List(r.Context(), "", 0)
	if err != nil {
		s.log.Error("list files", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list files")
		return
	}

	out := []reusableFile{}
	seen := map[string]struct{}{}
	for _, j := range list {
		if j.Status == jobs.StatusDone {
			if f, ok := describeFile(j, "output"); ok {
				if _, dup := seen[j.OutputPath]; !dup {
					seen[j.OutputPath] = struct{}{}
					out = append(out, f)
				}
			}
		}
		if f, ok := describeFile(j, "input"); ok {
			if _, dup := seen[j.InputPath]; !dup {
				seen[j.InputPath] = struct{}{}
				out = append(out, f)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

func (s *server) handleReuseFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	job, ok := s.lookup(w, r)
	if !ok {
		return
	}

	file, ok := describeFile(job, req.Source)
	if !ok {
		writeError(w, http.StatusNotFound, "file_missing", "that file is no longer on disk")
		return
	}

	stored, size, err := s.linkUpload(filePath(job, req.Source), file.Name)
	if err != nil {
		s.log.Error("reuse file", "id", job.ID, "source", req.Source, "err", err)
		writeError(w, http.StatusInternalServerError, "upload_failed", "could not reuse that file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id": stored,
		"filename":  file.Name,
		"size":      size,
	})
}

func filePath(j *jobs.Job, source string) string {
	switch source {
	case "output":
		return j.OutputPath
	case "input":
		return j.InputPath
	}
	return ""
}

func describeFile(j *jobs.Job, source string) (reusableFile, bool) {
	path := filePath(j, source)
	if path == "" {
		return reusableFile{}, false
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return reusableFile{}, false
	}
	name := originalName(filepath.Base(path))
	if source == "output" && j.OutputName != "" {
		name = j.OutputName
	}
	return reusableFile{
		JobID:     j.ID,
		Source:    source,
		Name:      name,
		Size:      st.Size(),
		CreatedAt: j.CreatedAt,
	}, true
}
