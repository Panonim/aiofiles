package jobs

import "sync"

type EventKind string

const (
	EventCreated  EventKind = "created"
	EventProgress EventKind = "progress"
	EventStatus   EventKind = "status"
	EventDeleted  EventKind = "deleted"
)

type Event struct {
	Kind     EventKind `json:"kind"`
	JobID    string    `json:"job_id"`
	Job      *Job      `json:"job,omitempty"`
	Progress *Progress `json:"progress,omitempty"`
}

// Bus fans events out to connected SSE clients. Publishing never blocks a
// worker: a subscriber that cannot keep up drops the event and reconciles on
// reconnect by re-fetching the job list.
type Bus struct {
	mu   sync.RWMutex
	next int
	subs map[int]chan Event
}

func NewBus() *Bus { return &Bus{subs: make(map[int]chan Event)} }

// Subscribe returns a channel and a cancel func the caller must invoke once
// the subscriber goes away.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}

func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow client: drop rather than block the worker
		}
	}
}

func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
