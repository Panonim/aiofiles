package jobs

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aiofiles/internal/db"
)

func newStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	handle, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return NewStore(handle), handle
}

func TestStoreJobLifecycle(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t)

	expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)
	job := &Job{
		Type:      TypeDownload,
		Title:     "A video",
		Source:    "https://example.com/watch?v=abc",
		Params:    []byte(`{"mode":"video"}`),
		ExpiresAt: &expires,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if job.ID == "" {
		t.Fatal("Create did not assign an id")
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}
	if job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		t.Error("Create did not stamp the timestamps")
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != TypeDownload || got.Title != "A video" || got.Source != job.Source {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if string(got.Params) != `{"mode":"video"}` {
		t.Errorf("params = %s", got.Params)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, expires)
	}
	if got.StartedAt != nil || got.FinishedAt != nil {
		t.Error("a queued job should have no start or finish time")
	}

	if err := store.SetStatus(ctx, job.ID, StatusRunning, ""); err != nil {
		t.Fatalf("SetStatus(running): %v", err)
	}
	got, err = store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("running did not set started_at")
	}
	if got.FinishedAt != nil {
		t.Error("running set finished_at")
	}

	if err := store.SetProgress(ctx, job.ID, 42.5, "downloading"); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	if err := store.SetOutput(ctx, job.ID, "/data/downloads/a-video-abcd1234.mp4", "Real Title", 1234); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if err := store.SetStatus(ctx, job.ID, StatusDone, ""); err != nil {
		t.Fatalf("SetStatus(done): %v", err)
	}

	got, err = store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress != 42.5 || got.Stage != "downloading" {
		t.Errorf("progress = %v %q", got.Progress, got.Stage)
	}
	if got.OutputSize != 1234 || got.OutputName != "a-video-abcd1234.mp4" {
		t.Errorf("output = %d %q", got.OutputSize, got.OutputName)
	}
	if got.Title != "Real Title" {
		t.Errorf("title = %q, want the runner's title", got.Title)
	}
	if got.Status != StatusDone || got.FinishedAt == nil {
		t.Errorf("status = %q, finished_at = %v", got.Status, got.FinishedAt)
	}
	// The started_at stamp must survive later updates.
	if got.StartedAt == nil {
		t.Error("done cleared started_at")
	}

	if err := store.Delete(ctx, job.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestStoreGetMissing(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Get(t.Context(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}

func TestStoreCreateDefaults(t *testing.T) {
	store, _ := newStore(t)
	job := &Job{Type: TypeImage}
	if err := store.Create(t.Context(), job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The params column is NOT NULL and runners unmarshal it unconditionally.
	if string(got.Params) != "{}" {
		t.Errorf("params = %q, want {}", got.Params)
	}
	if got.Status != StatusQueued {
		t.Errorf("status = %q", got.Status)
	}
	if got.OutputName != "" {
		t.Errorf("output name = %q, want empty for a job with no output", got.OutputName)
	}
}

func TestStoreList(t *testing.T) {
	ctx := t.Context()
	store, handle := newStore(t)

	if list, err := store.List(ctx, "", 0); err != nil || len(list) != 0 {
		t.Fatalf("List on an empty store = %v, %v", list, err)
	}

	base := time.Now().UTC().Add(-time.Hour)
	ids := make([]string, 3)
	for i := range ids {
		j := &Job{Type: TypeConvert, Title: "job"}
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[i] = j.ID
		// Created within the same millisecond otherwise, which leaves the
		// newest-first ordering undecided.
		if _, err := handle.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			base.Add(time.Duration(i)*time.Minute).UnixMilli(), j.ID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}
	if err := store.SetStatus(ctx, ids[0], StatusDone, ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	list, err := store.List(ctx, "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List returned %d jobs, want 3", len(list))
	}
	if list[0].ID != ids[2] || list[2].ID != ids[0] {
		t.Errorf("List is not newest first: %q", []string{list[0].ID, list[1].ID, list[2].ID})
	}

	done, err := store.List(ctx, StatusDone, 0)
	if err != nil {
		t.Fatalf("List(done): %v", err)
	}
	if len(done) != 1 || done[0].ID != ids[0] {
		t.Errorf("List(done) = %v, want just %s", done, ids[0])
	}

	limited, err := store.List(ctx, "", 2)
	if err != nil {
		t.Fatalf("List(limit 2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit ignored: got %d jobs", len(limited))
	}
}

// created_at only has millisecond resolution, so a burst of submissions shares
// a timestamp; the order must still be stable and newest first.
func TestStoreListSameMillisecondIsDeterministic(t *testing.T) {
	ctx := t.Context()
	store, handle := newStore(t)

	same := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	ids := make([]string, 5)
	for i := range ids {
		j := &Job{Type: TypeConvert, Title: "job"}
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[i] = j.ID
		if _, err := handle.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			same.UnixMilli(), j.ID); err != nil {
			t.Fatalf("collapse created_at: %v", err)
		}
	}

	// Newest first means last inserted first when the timestamps are equal.
	want := make([]string, len(ids))
	for i, id := range ids {
		want[len(ids)-1-i] = id
	}

	check := func(label string, got []*Job, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s returned %d jobs, want %d", label, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("%s[%d] = %s, want %s", label, i, got[i].ID, want[i])
			}
		}
	}

	for range 5 {
		list, err := store.List(ctx, "", 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		check("List", list, want)

		// The limit has to cut from the same end, or paging would skip jobs.
		limited, err := store.List(ctx, "", 2)
		if err != nil {
			t.Fatalf("List(limit 2): %v", err)
		}
		check("List(limit 2)", limited, want[:2])

		// The filtered query uses a different index and must agree.
		filtered, err := store.List(ctx, StatusQueued, 0)
		if err != nil {
			t.Fatalf("List(queued): %v", err)
		}
		check("List(queued)", filtered, want)
	}
}

func TestStoreExpired(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t)
	now := time.Now().UTC()

	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	stale := &Job{Type: TypeImage, ExpiresAt: &past}
	fresh := &Job{Type: TypeImage, ExpiresAt: &future}
	forever := &Job{Type: TypeImage}
	for _, j := range []*Job{stale, fresh, forever} {
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	expired, err := store.Expired(ctx, now)
	if err != nil {
		t.Fatalf("Expired: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != stale.ID {
		t.Fatalf("Expired returned %d jobs, want only the stale one", len(expired))
	}
}

// An unclean shutdown leaves rows claiming to be running; their processes are
// gone. Queued rows are untouched - Queue.Resume picks those up.
func TestFailInterrupted(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t)

	statuses := []Status{StatusRunning, StatusQueued, StatusDone, StatusFailed, StatusCanceled}
	ids := make(map[Status]string, len(statuses))
	for _, st := range statuses {
		j := &Job{Type: TypeConvert}
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.SetStatus(ctx, j.ID, st, ""); err != nil {
			t.Fatalf("SetStatus(%s): %v", st, err)
		}
		ids[st] = j.ID
	}

	n, err := store.FailInterrupted(ctx)
	if err != nil {
		t.Fatalf("FailInterrupted: %v", err)
	}
	if n != 1 {
		t.Errorf("FailInterrupted reported %d rows, want 1", n)
	}

	got, err := store.Get(ctx, ids[StatusRunning])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("the running job is now %q, want failed", got.Status)
	}
	if got.Error == "" {
		t.Error("the running job carries no explanation")
	}

	for _, st := range []Status{StatusQueued, StatusDone, StatusFailed, StatusCanceled} {
		got, err := store.Get(ctx, ids[st])
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != st {
			t.Errorf("a %s job was changed to %q", st, got.Status)
		}
	}

	if n, err := store.FailInterrupted(ctx); err != nil || n != 0 {
		t.Errorf("second FailInterrupted = %d, %v; want 0, nil", n, err)
	}
}

func TestStoreQueuedIDsOldestFirst(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t)

	var want []string
	for range 3 {
		j := &Job{Type: TypeConvert}
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		want = append(want, j.ID)
	}
	other := &Job{Type: TypeConvert}
	if err := store.Create(ctx, other); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetStatus(ctx, other.ID, StatusRunning, ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	got, err := store.QueuedIDs(ctx)
	if err != nil {
		t.Fatalf("QueuedIDs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("QueuedIDs = %v, want the %d queued jobs", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("QueuedIDs = %v, want %v (oldest first)", got, want)
		}
	}
}

// fakeRunner stands in for yt-dlp/ffmpeg: it writes a file and reports progress.
type fakeRunner struct {
	dir     string
	fail    error
	started chan struct{} // buffered: only the first Run needs to be observed
	release chan struct{}
	seen    chan string // records execution order, one entry per Run
	partial bool        // leave a half-written output behind when interrupted
}

func (f *fakeRunner) Run(ctx context.Context, j *Job, emit Emit) (Result, error) {
	if f.seen != nil {
		f.seen <- j.ID
	}
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			if !f.partial {
				return Result{}, ctx.Err()
			}
			out := filepath.Join(f.dir, j.ID+".part")
			if err := os.WriteFile(out, []byte("half"), 0o600); err != nil {
				return Result{}, err
			}
			return Result{OutputPath: out}, ctx.Err()
		}
	}
	if f.fail != nil {
		return Result{}, f.fail
	}
	emit(Progress{Percent: 50, Stage: "encoding"})
	out := filepath.Join(f.dir, j.ID+".out")
	if err := os.WriteFile(out, []byte("output"), 0o600); err != nil {
		return Result{}, err
	}
	return Result{OutputPath: out, Title: "Finished"}, nil
}

func waitForStatus(t *testing.T, store *Store, id string, want Status) *Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := store.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if j.Status == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s is %q after 10s, want %q (%s)", id, j.Status, want, j.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestQueueRunsJobToCompletion(t *testing.T) {
	store, _ := newStore(t)
	bus := NewBus()
	q := NewQueue(store, bus, slog.New(slog.DiscardHandler), 4)
	q.Register(TypeConvert, &fakeRunner{dir: t.TempDir()})

	events, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	q.Start(t.Context(), 1)
	defer q.Stop()

	job := &Job{Type: TypeConvert, Title: "clip.mov"}
	if err := q.Submit(t.Context(), job); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	done := waitForStatus(t, store, job.ID, StatusDone)
	if done.OutputPath == "" || done.OutputSize != int64(len("output")) {
		t.Errorf("output not recorded: path=%q size=%d", done.OutputPath, done.OutputSize)
	}
	if done.Title != "Finished" {
		t.Errorf("title = %q, want the runner's title", done.Title)
	}
	if done.Progress != 100 {
		t.Errorf("progress = %v, want 100", done.Progress)
	}

	kinds := map[EventKind]bool{}
	for len(events) > 0 {
		kinds[(<-events).Kind] = true
	}
	if !kinds[EventCreated] || !kinds[EventStatus] {
		t.Errorf("bus events = %v, want at least created and status", kinds)
	}
}

func TestQueueFailedJobIsRecorded(t *testing.T) {
	store, _ := newStore(t)
	q := NewQueue(store, NewBus(), slog.New(slog.DiscardHandler), 4)
	q.Register(TypeImage, &fakeRunner{dir: t.TempDir(), fail: errors.New("magick exploded")})

	q.Start(t.Context(), 1)
	defer q.Stop()

	job := &Job{Type: TypeImage}
	if err := q.Submit(t.Context(), job); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	failed := waitForStatus(t, store, job.ID, StatusFailed)
	if failed.Error == "" {
		t.Error("a failed job carries no error text")
	}
}

func TestQueueSubmitWithoutRunner(t *testing.T) {
	store, _ := newStore(t)
	q := NewQueue(store, NewBus(), slog.New(slog.DiscardHandler), 4)

	job := &Job{Type: TypeDownload}
	if err := q.Submit(t.Context(), job); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Submit = %v, want ErrNoRunner", err)
	}
	if list, err := store.List(t.Context(), "", 0); err != nil || len(list) != 0 {
		t.Errorf("a job with no runner was persisted anyway: %v, %v", list, err)
	}
}

// A restart leaves rows queued; Resume must run them, oldest first.
func TestQueueResumeRunsJobsLeftQueued(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t)
	seen := make(chan string, 3)
	q := NewQueue(store, NewBus(), slog.New(slog.DiscardHandler), 4)
	q.Register(TypeConvert, &fakeRunner{dir: t.TempDir(), seen: seen})

	var queued []string
	for range 3 {
		j := &Job{Type: TypeConvert}
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		queued = append(queued, j.ID)
	}
	// A job left "running" is not resumable and must not be picked up.
	orphan := &Job{Type: TypeConvert}
	if err := store.Create(ctx, orphan); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetStatus(ctx, orphan.ID, StatusRunning, ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	n, err := q.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != len(queued) {
		t.Fatalf("Resume re-enqueued %d jobs, want %d", n, len(queued))
	}

	q.Start(ctx, 1) // one worker, so execution order is the enqueue order
	defer q.Stop()

	for i, want := range queued {
		select {
		case got := <-seen:
			if got != want {
				t.Fatalf("job %d to run = %s, want %s (oldest first)", i, got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d resumed jobs ran", i, len(queued))
		}
	}
	for _, id := range queued {
		waitForStatus(t, store, id, StatusDone)
	}
	if got, err := store.Get(ctx, orphan.ID); err != nil || got.Status != StatusRunning {
		t.Errorf("the orphaned running job = %v, %v; Resume must ignore it", got.Status, err)
	}
}

// Resume is bound by the same QUEUE_DEPTH as Submit, and overflow is visible.
func TestQueueResumeRespectsDepth(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t)
	q := NewQueue(store, NewBus(), slog.New(slog.DiscardHandler), 1)
	q.Register(TypeConvert, &fakeRunner{dir: t.TempDir()})

	var queued []string
	for range 3 {
		j := &Job{Type: TypeConvert}
		if err := store.Create(ctx, j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		queued = append(queued, j.ID)
	}

	// No workers are started, so nothing drains the single buffer slot.
	n, err := q.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("Resume re-enqueued %d jobs into a queue of depth 1", n)
	}

	if got, err := store.Get(ctx, queued[0]); err != nil || got.Status != StatusQueued {
		t.Errorf("the oldest job = %v, %v; want it still queued", got.Status, err)
	}
	for _, id := range queued[1:] {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != StatusFailed || got.Error != ErrQueueFull.Error() {
			t.Errorf("overflow job = %q/%q, want failed with %q", got.Status, got.Error, ErrQueueFull)
		}
	}
}

func TestQueueFullSubmitIsVisible(t *testing.T) {
	store, _ := newStore(t)
	runner := &fakeRunner{dir: t.TempDir(), started: make(chan struct{}, 1), release: make(chan struct{})}
	q := NewQueue(store, NewBus(), slog.New(slog.DiscardHandler), 1)
	q.Register(TypeConvert, runner)

	q.Start(t.Context(), 1)
	defer func() {
		close(runner.release)
		q.Stop()
	}()

	// One job occupies the worker, the next fills the single queue slot, and the
	// third has nowhere to go.
	first := &Job{Type: TypeConvert}
	if err := q.Submit(t.Context(), first); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-runner.started

	var lastErr error
	var last *Job
	for range 3 {
		last = &Job{Type: TypeConvert}
		if lastErr = q.Submit(t.Context(), last); lastErr != nil {
			break
		}
	}
	if !errors.Is(lastErr, ErrQueueFull) {
		t.Fatalf("Submit = %v, want ErrQueueFull once the buffer is full", lastErr)
	}

	// The rejected job still exists, marked failed, so the UI can show it.
	got, err := store.Get(t.Context(), last.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}
