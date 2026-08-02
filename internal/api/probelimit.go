package api

import (
	"sync"
	"time"
)

// /api/probe forks a yt-dlp process per request, outside the job queue, so
// MAX_CONCURRENT_JOBS does not bound it. The limits are global rather than
// per-client: behind the bundled nginx every request comes from 127.0.0.1 and
// the forwarded headers are client-supplied.
const (
	probeBurst    = 8
	probeInterval = 3 * time.Second // one token per
	maxProbeSlots = 4
)

type probeLimiter struct {
	slots chan struct{}

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newProbeLimiter(inFlight int) *probeLimiter {
	if inFlight < 1 {
		inFlight = 1
	}
	if inFlight > maxProbeSlots {
		inFlight = maxProbeSlots
	}
	return &probeLimiter{
		slots:  make(chan struct{}, inFlight),
		tokens: probeBurst,
		last:   time.Now(),
	}
}

// Never blocks: an over-limit request is refused rather than queued, so a flood
// cannot pile up handlers holding 32 MiB buffers each.
func (l *probeLimiter) acquire() (release func(), ok bool) {
	if !l.take() {
		return nil, false
	}
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, true
	default:
		l.refund()
		return nil, false
	}
}

func (l *probeLimiter) take() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() / probeInterval.Seconds()
	if l.tokens > probeBurst {
		l.tokens = probeBurst
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// A request the in-flight cap refused should not also cost a token.
func (l *probeLimiter) refund() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tokens < probeBurst {
		l.tokens++
	}
}
