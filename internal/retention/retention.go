// Package retention deletes jobs whose retention window has elapsed, along
// with the files they own.
package retention

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"aiofiles/internal/jobs"
)

// DefaultInterval is the sweep period used when the caller passes 0.
const DefaultInterval = 10 * time.Minute

// DefaultUploadGrace is how long an upload no job refers to is kept before it
// is unlinked. An upload only becomes garbage if the client never submitted the
// job, which normally happens seconds after the transfer ends; a full day of
// slack covers a browser tab left open overnight and any clock skew, while
// still bounding how much abandoned data can accumulate.
const DefaultUploadGrace = 24 * time.Hour

type Sweeper struct {
	store    *jobs.Store
	bus      *jobs.Bus
	log      *slog.Logger
	interval time.Duration

	uploadDir   string
	uploadGrace time.Duration
}

func NewSweeper(store *jobs.Store, bus *jobs.Bus, log *slog.Logger, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{store: store, bus: bus, log: log, interval: interval}
}

// WithUploadGC makes every sweep also unlink files in dir that no job refers to
// and that were last written more than grace ago. A grace of 0 or less turns
// the collection off.
func (s *Sweeper) WithUploadGC(dir string, grace time.Duration) *Sweeper {
	s.uploadDir, s.uploadGrace = dir, grace
	return s
}

// Start runs the sweep loop in the background and returns immediately.
func (s *Sweeper) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := s.SweepOnce(ctx); err != nil {
					s.log.Warn("retention sweep failed", "err", err)
				}
			}
		}
	}()
}

// SweepOnce deletes every job past its expiry and returns how many rows went
// away. Files that are already gone are not an error.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	expired, err := s.store.Expired(ctx, time.Now())
	if err != nil {
		return 0, err
	}

	var (
		deleted int
		freed   int64
	)
	for _, j := range expired {
		s.remove(j.OutputPath)
		s.removeInput(ctx, j)

		if err := s.store.Delete(ctx, j.ID); err != nil {
			s.log.Warn("delete expired job", "id", j.ID, "err", err)
			continue
		}
		deleted++
		freed += j.OutputSize
		if s.bus != nil {
			s.bus.Publish(jobs.Event{Kind: jobs.EventDeleted, JobID: j.ID})
		}
	}

	// Runs after the job pass, so inputs freed above are collected in the same
	// sweep once they are old enough.
	uploads, uploadBytes := s.sweepUploads(ctx)
	freed += uploadBytes

	// Silent on an empty sweep: this runs forever in the background.
	if deleted > 0 || uploads > 0 {
		s.log.Info("retention sweep", "deleted", deleted, "uploads_removed", uploads,
			"bytes_freed", freed)
	}
	return deleted, nil
}

// sweepUploads unlinks orphaned uploads: files in the upload directory that no
// job row points at and that nobody has written to for uploadGrace. An upload
// that a client is about to turn into a job has no row yet, so the age check is
// the only thing protecting it - and it uses the mtime of a file the server
// wrote itself, which no uploader can backdate.
func (s *Sweeper) sweepUploads(ctx context.Context) (int, int64) {
	if s.uploadDir == "" || s.uploadGrace <= 0 {
		return 0, 0
	}
	entries, err := os.ReadDir(s.uploadDir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Warn("read upload dir", "dir", s.uploadDir, "err", err)
		}
		return 0, 0
	}
	referenced, err := s.store.InputPaths(ctx)
	if err != nil {
		// Without the reference set every upload looks orphaned; skip this round.
		s.log.Warn("list referenced uploads", "err", err)
		return 0, 0
	}
	// The API stores input_path with symlinks already resolved, so resolve the
	// directory the same way before comparing. Basenames are a second guard: an
	// upload name carries 16 hex characters of entropy, so a match there means
	// the same file however the two paths were spelled.
	dir := realDir(s.uploadDir)
	names := make(map[string]struct{}, len(referenced))
	for p := range referenced {
		names[filepath.Base(p)] = struct{}{}
	}

	cutoff := time.Now().Add(-s.uploadGrace)
	var (
		removed int
		freed   int64
	)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if _, ok := names[e.Name()]; ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if _, ok := referenced[path]; ok {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.log.Warn("remove orphaned upload", "path", path, "err", err)
			continue
		}
		removed++
		freed += info.Size()
	}
	return removed, freed
}

// removeInput unlinks an expired job's upload unless another job was created
// from the same file. The survivor's own expiry - or the orphan sweep above -
// eventually removes it.
func (s *Sweeper) removeInput(ctx context.Context, j *jobs.Job) {
	if j.InputPath == "" {
		return
	}
	shared, err := s.store.InputPathShared(ctx, j.InputPath, j.ID)
	if err != nil {
		s.log.Warn("check shared input", "id", j.ID, "err", err)
		return // uncertain: leave the file to the orphan sweep
	}
	if shared {
		return
	}
	s.remove(j.InputPath)
}

func realDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func (s *Sweeper) remove(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.log.Warn("remove expired file", "path", path, "err", err)
	}
}
