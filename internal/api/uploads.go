package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Stored uploads are named "<random-hex>-<sanitised original>", and that whole
// basename is the opaque upload_id handed back to the client: the mapping stays
// stateless and two users can both upload "video.mp4".
const (
	uploadTokenBytes = 8
	maxStoredName    = 120 // runes, before the token prefix
)

var errBadUploadID = errors.New("invalid upload id")

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	// MAX_UPLOAD_MIB=0 means unlimited here and in nginx's client_max_body_size;
	// config.Load rejects anything below 0.
	limit := s.cfg.MaxUploadBytes()
	if limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_multipart", "expected a multipart/form-data body")
		return
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_multipart", "could not read the upload")
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}

		original := sanitiseFilename(part.FileName())
		storedName, size, err := s.storeUpload(part, original)
		_ = part.Close()
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
					fmt.Sprintf("file exceeds the %d byte upload limit", limit))
				return
			}
			s.log.Error("store upload", "err", err)
			writeError(w, http.StatusInternalServerError, "upload_failed", "could not store the upload")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"upload_id": storedName,
			"filename":  original,
			"size":      size,
		})
		return
	}

	writeError(w, http.StatusBadRequest, "missing_file", `multipart form field "file" is required`)
}

// Streams the part straight to its final location: a multi-GiB file is never
// buffered in memory or copied twice.
func (s *server) storeUpload(src io.Reader, original string) (string, int64, error) {
	var token [uploadTokenBytes]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", 0, err
	}
	stored := hex.EncodeToString(token[:]) + "-" + original

	path := filepath.Join(s.cfg.UploadDir, stored)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	return stored, n, nil
}

// resolveUpload maps a client-controlled upload_id back to a path inside
// cfg.UploadDir: it must be a bare basename, and the resolved path must still
// sit inside the upload directory once symlinks are evaluated.
func (s *server) resolveUpload(id string) (string, error) {
	bad := id == "" || len(id) > 512 ||
		strings.ContainsAny(id, `/\`) || strings.ContainsRune(id, 0) ||
		id == "." || strings.Contains(id, "..") ||
		filepath.IsAbs(id) || filepath.Clean(id) != id || filepath.Base(id) != id
	if bad {
		return "", errBadUploadID
	}

	dir, err := evalDir(s.cfg.UploadDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, id)

	// Resolve symlinks before the containment check: a symlinked upload must not
	// be able to point at /etc/passwd.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errBadUploadID
	}
	if real != dir && !strings.HasPrefix(real, dir+string(os.PathSeparator)) {
		return "", errBadUploadID
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return "", errBadUploadID
	}
	return real, nil
}

func evalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return filepath.Clean(abs), nil
}

func originalName(stored string) string {
	if i := strings.IndexByte(stored, '-'); i == uploadTokenBytes*2 {
		return stored[i+1:]
	}
	return stored
}

// Reduces a client-supplied name to something safe both in a directory and as
// an argv element for a child process. Unicode survives; separators, control
// characters and shell-hostile bytes do not.
func sanitiseFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "")
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		// Space first: tab and newline are also control characters, and they
		// should become "_" rather than vanish.
		case r == '/' || r == os.PathSeparator, unicode.IsSpace(r),
			strings.ContainsRune(`:*?"<>|`+"`"+`$;&'\`, r):
			b.WriteByte('_')
		case unicode.IsControl(r), r == utf8.RuneError:
			// drop
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" || out == "." || out == ".." {
		return "upload"
	}

	// Keep the extension when truncating: runners key off it.
	if utf8.RuneCountInString(out) > maxStoredName {
		ext := filepath.Ext(out)
		if utf8.RuneCountInString(ext) > 16 {
			ext = ""
		}
		stem := []rune(strings.TrimSuffix(out, ext))
		keep := maxStoredName - utf8.RuneCountInString(ext)
		if len(stem) > keep {
			stem = stem[:keep]
		}
		out = strings.Trim(string(stem), "._-") + ext
	}
	if out == "" {
		return "upload"
	}
	return out
}
