package api

import "testing"

func TestProbeLimiterBurstThenRefusal(t *testing.T) {
	l := newProbeLimiter(maxProbeSlots)

	// Released immediately, so only the token bucket is under test here.
	for i := 0; i < probeBurst; i++ {
		release, ok := l.acquire()
		if !ok {
			t.Fatalf("acquire %d refused inside the burst", i)
		}
		release()
	}
	if _, ok := l.acquire(); ok {
		t.Fatal("acquire past the burst succeeded")
	}
}

func TestProbeLimiterCapsInFlight(t *testing.T) {
	l := newProbeLimiter(2)

	a, ok := l.acquire()
	if !ok {
		t.Fatal("first acquire refused")
	}
	b, ok := l.acquire()
	if !ok {
		t.Fatal("second acquire refused")
	}
	if _, ok := l.acquire(); ok {
		t.Fatal("third concurrent acquire succeeded, want refusal")
	}

	a()
	// The refused attempt must not have burned a token.
	c, ok := l.acquire()
	if !ok {
		t.Fatal("acquire after release refused")
	}
	b()
	c()
}

func TestProbeLimiterClampsSlots(t *testing.T) {
	if got := cap(newProbeLimiter(0).slots); got != 1 {
		t.Errorf("slots for 0 = %d, want 1", got)
	}
	if got := cap(newProbeLimiter(64).slots); got != maxProbeSlots {
		t.Errorf("slots for 64 = %d, want %d", got, maxProbeSlots)
	}
}
