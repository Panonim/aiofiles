package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

var (
	ErrQueueFull     = errors.New("job queue is full")
	ErrNoRunner      = errors.New("no runner registered for job type")
	ErrNotCancelable = errors.New("job is not running")
)

// Cause attached to a job context killed by the wall-clock limit, so the
// timeout can be told apart from an operator pressing cancel.
var errTimedOut = errors.New("job time limit exceeded")

// Throttles so a chatty ffmpeg cannot pin the disk or the event loop; bus
// events are cheaper than SQLite writes and go out more often.
const (
	dbProgressInterval  = 2 * time.Second
	busProgressInterval = 400 * time.Millisecond
)

// Queue is a fixed worker pool fed by a buffered channel.
type Queue struct {
	store   *Store
	bus     *Bus
	log     *slog.Logger
	runners map[Type]Runner

	ch      chan string
	wg      sync.WaitGroup
	timeout time.Duration

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func NewQueue(store *Store, bus *Bus, log *slog.Logger, depth int) *Queue {
	if depth <= 0 {
		depth = 256
	}
	return &Queue{
		store:   store,
		bus:     bus,
		log:     log,
		runners: make(map[Type]Runner),
		ch:      make(chan string, depth),
		running: make(map[string]context.CancelFunc),
	}
}

// Register wires a Runner to a job type. Call before Start.
func (q *Queue) Register(t Type, r Runner) { q.runners[t] = r }

// SetTimeout bounds how long a single job may occupy a worker; zero or less
// means no limit. Call before Start.
func (q *Queue) SetTimeout(d time.Duration) { q.timeout = d }

// Start launches n workers. They exit when ctx is canceled or Stop is called.
func (q *Queue) Start(ctx context.Context, n int) {
	for range n {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id, ok := <-q.ch:
					if !ok {
						return
					}
					q.execute(ctx, id)
				}
			}
		}()
	}
}

// Stop closes the intake channel and waits for in-flight jobs to unwind.
func (q *Queue) Stop() {
	close(q.ch)
	q.wg.Wait()
}

// Submit persists the job and enqueues it. The job is created even if the
// queue is full so the failure is visible in the UI.
func (q *Queue) Submit(ctx context.Context, j *Job) error {
	if _, ok := q.runners[j.Type]; !ok {
		return fmt.Errorf("%w: %s", ErrNoRunner, j.Type)
	}
	j.Status = StatusQueued
	if err := q.store.Create(ctx, j); err != nil {
		return err
	}
	q.bus.Publish(Event{Kind: EventCreated, JobID: j.ID, Job: j})

	select {
	case q.ch <- j.ID:
		return nil
	default:
		_ = q.store.SetStatus(ctx, j.ID, StatusFailed, ErrQueueFull.Error())
		q.publishStatus(ctx, j.ID)
		return ErrQueueFull
	}
}

// Resume re-enqueues jobs that were still queued when the previous process
// exited: nothing of theirs ran, and their params are fully persisted. Call it
// once at startup, before Start and before the server accepts requests, so the
// buffer is empty and no submission can race it. Jobs that do not fit in the
// buffer are failed exactly as Submit fails them.
func (q *Queue) Resume(ctx context.Context) (int, error) {
	ids, err := q.store.QueuedIDs(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, id := range ids {
		select {
		case q.ch <- id:
			n++
		default:
			_ = q.store.SetStatus(ctx, id, StatusFailed, ErrQueueFull.Error())
			q.publishStatus(ctx, id)
		}
	}
	return n, nil
}

func (q *Queue) Cancel(id string) error {
	q.mu.Lock()
	cancel, ok := q.running[id]
	q.mu.Unlock()
	if !ok {
		return ErrNotCancelable
	}
	cancel()
	return nil
}

func (q *Queue) execute(parent context.Context, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	job, err := q.store.Get(ctx, id)
	if err != nil {
		q.log.Error("load job", "id", id, "err", err)
		return
	}
	runner, ok := q.runners[job.Type]
	if !ok {
		q.fail(ctx, job, fmt.Errorf("%w: %s", ErrNoRunner, job.Type))
		return
	}

	q.mu.Lock()
	q.running[id] = cancel
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.running, id)
		q.mu.Unlock()
	}()

	// The wall-clock limit hangs off the cancelable context registered above, so
	// Cancel still unwinds the job and the runner's process group either way.
	if q.timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeoutCause(ctx, q.timeout, errTimedOut)
		defer stop()
	}

	if err := q.store.SetStatus(ctx, id, StatusRunning, ""); err != nil {
		q.log.Error("mark running", "id", id, "err", err)
	}
	q.publishStatus(ctx, id)

	var (
		lastDB  time.Time
		lastBus time.Time
		mu      sync.Mutex
	)
	emit := func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if now.Sub(lastDB) >= dbProgressInterval {
			lastDB = now
			// Detached context: progress must still land if the job was canceled.
			if err := q.store.SetProgress(context.WithoutCancel(ctx), id, p.Percent, p.Stage); err != nil {
				q.log.Debug("persist progress", "id", id, "err", err)
			}
		}
		if now.Sub(lastBus) >= busProgressInterval || p.Percent >= 100 {
			lastBus = now
			q.bus.Publish(Event{Kind: EventProgress, JobID: id, Progress: &p})
		}
	}

	res, runErr := runner.Run(ctx, job, emit)

	// The job outlives its own cancellation for bookkeeping purposes.
	fin := context.WithoutCancel(ctx)
	if runErr != nil {
		q.cleanup(res.OutputPath)
		if errors.Is(context.Cause(ctx), errTimedOut) {
			q.fail(fin, job, fmt.Errorf(
				"job stopped after hitting the %s time limit (JOB_TIMEOUT_MINUTES)", q.timeout))
			return
		}
		if ctx.Err() != nil {
			_ = q.store.SetStatus(fin, id, StatusCanceled, "canceled")
			q.publishStatus(fin, id)
			return
		}
		q.fail(fin, job, runErr)
		return
	}

	st, err := os.Stat(res.OutputPath)
	if err != nil {
		q.fail(fin, job, fmt.Errorf("output missing: %w", err))
		return
	}
	if err := q.store.SetOutput(fin, id, res.OutputPath, res.Title, st.Size()); err != nil {
		q.log.Error("record output", "id", id, "err", err)
	}
	_ = q.store.SetProgress(fin, id, 100, "done")
	_ = q.store.SetStatus(fin, id, StatusDone, "")
	q.publishStatus(fin, id)
}

func (q *Queue) fail(ctx context.Context, j *Job, err error) {
	q.log.Warn("job failed", "id", j.ID, "type", j.Type, "err", err)
	_ = q.store.SetStatus(ctx, j.ID, StatusFailed, err.Error())
	q.publishStatus(ctx, j.ID)
}

func (q *Queue) cleanup(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		q.log.Debug("remove partial output", "path", path, "err", err)
	}
}

func (q *Queue) publishStatus(ctx context.Context, id string) {
	j, err := q.store.Get(ctx, id)
	if err != nil {
		return
	}
	q.bus.Publish(Event{Kind: EventStatus, JobID: id, Job: j})
}
