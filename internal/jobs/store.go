package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNotFound = errors.New("job not found")

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const jobCols = `id, type, title, source, params, input_path, output_path, output_size,
	status, progress, stage, error, created_at, updated_at, started_at, finished_at, expires_at`

func (s *Store) Create(ctx context.Context, j *Job) error {
	now := time.Now().UTC()
	if j.ID == "" {
		j.ID = NewID()
	}
	j.CreatedAt, j.UpdatedAt = now, now
	if j.Status == "" {
		j.Status = StatusQueued
	}
	if len(j.Params) == 0 {
		j.Params = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (`+jobCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, string(j.Type), j.Title, j.Source, string(j.Params), j.InputPath,
		j.OutputPath, j.OutputSize, string(j.Status), j.Progress, j.Stage, j.Error,
		j.CreatedAt.UnixMilli(), j.UpdatedAt.UnixMilli(),
		nullMillis(j.StartedAt), nullMillis(j.FinishedAt), nullMillis(j.ExpiresAt))
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// List returns jobs newest first, ties broken by insertion order (newest
// first there too). Pass an empty status to list everything.
func (s *Store) List(ctx context.Context, status Status, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + jobCols + ` FROM jobs`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, string(status))
	}
	// created_at has millisecond resolution, so rapid submissions collide;
	// rowid is the insertion sequence and keeps the order stable.
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return scanJobs(rows)
}

// SetStatus updates status plus the matching timestamp and error text.
func (s *Store) SetStatus(ctx context.Context, id string, st Status, errText string) error {
	now := time.Now().UTC()
	var started, finished any
	switch {
	case st == StatusRunning:
		started = now.UnixMilli()
	case st.Terminal():
		finished = now.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		status = ?, error = ?, updated_at = ?,
		started_at  = COALESCE(?, started_at),
		finished_at = COALESCE(?, finished_at)
		WHERE id = ?`,
		string(st), errText, now.UnixMilli(), started, finished, id)
	return err
}

func (s *Store) SetProgress(ctx context.Context, id string, pct float64, stage string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET progress = ?, stage = ?, updated_at = ? WHERE id = ?`,
		pct, stage, time.Now().UTC().UnixMilli(), id)
	return err
}

func (s *Store) SetOutput(ctx context.Context, id, path, name string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET output_path = ?, output_size = ?, title = COALESCE(NULLIF(?,''), title),
		 updated_at = ? WHERE id = ?`,
		path, size, name, time.Now().UTC().UnixMilli(), id)
	return err
}

func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Expired returns jobs whose retention window has passed.
func (s *Store) Expired(ctx context.Context, now time.Time) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs
		WHERE expires_at IS NOT NULL AND expires_at <= ? AND status != ?`,
		now.UnixMilli(), string(StatusExpired))
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

// FailInterrupted fails jobs left running by an unclean shutdown - their child
// processes died with the old container, so there is nothing to resume. Jobs
// still queued are left alone; Queue.Resume re-enqueues them. Called once at
// startup, before anything can submit new work.
func (s *Store) FailInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, error = 'interrupted by restart', updated_at = ?
		 WHERE status = ?`,
		string(StatusFailed), time.Now().UTC().UnixMilli(), string(StatusRunning))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// InputPaths returns the set of upload paths currently referenced by a job.
// The retention sweeper uses it to tell an orphaned upload from a live input.
func (s *Store) InputPaths(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT input_path FROM jobs WHERE input_path != ''`)
	if err != nil {
		return nil, fmt.Errorf("list input paths: %w", err)
	}
	defer rows.Close()

	paths := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths[p] = struct{}{}
	}
	return paths, rows.Err()
}

// InputPathShared reports whether a job other than exceptID also uses path.
// Two jobs can be built from the same upload, so deleting one of them must not
// unlink an input the other still needs.
func (s *Store) InputPathShared(ctx context.Context, path, exceptID string) (bool, error) {
	if path == "" {
		return false, nil
	}
	var shared bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM jobs WHERE input_path = ? AND id != ?)`,
		path, exceptID).Scan(&shared)
	if err != nil {
		return false, fmt.Errorf("check shared input: %w", err)
	}
	return shared, nil
}

// QueuedIDs returns the ids of jobs still waiting to run, oldest first.
func (s *Store) QueuedIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM jobs WHERE status = ? ORDER BY created_at, rowid`,
		string(StatusQueued))
	if err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanJobs(rows *sql.Rows) ([]*Job, error) {
	defer rows.Close()

	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func scanJob(sc scanner) (*Job, error) {
	var (
		j                            Job
		typ, status, params          string
		started, finished, expires   sql.NullInt64
		createdMillis, updatedMillis int64
	)
	err := sc.Scan(&j.ID, &typ, &j.Title, &j.Source, &params, &j.InputPath,
		&j.OutputPath, &j.OutputSize, &status, &j.Progress, &j.Stage, &j.Error,
		&createdMillis, &updatedMillis, &started, &finished, &expires)
	if err != nil {
		return nil, err
	}
	j.Type, j.Status, j.Params = Type(typ), Status(status), []byte(params)
	j.CreatedAt = time.UnixMilli(createdMillis).UTC()
	j.UpdatedAt = time.UnixMilli(updatedMillis).UTC()
	j.StartedAt = millisPtr(started)
	j.FinishedAt = millisPtr(finished)
	j.ExpiresAt = millisPtr(expires)
	j.OutputName = baseName(j.OutputPath)
	return &j, nil
}

func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMilli()
}

func millisPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.UnixMilli(n.Int64).UTC()
	return &t
}

// baseName differs from path.Base only for "", which stays empty.
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
