package reloader

import (
	"sync"
	"testing"
	"time"
)

// awaitDeadline bounds cross-goroutine waits in tests. It only elapses on
// test failure, so it never adds real time to a passing run.
const awaitDeadline = 5 * time.Second

// fakeClock is the hand-rolled Clock for orchestration and router tests.
// Production code requests timers through After; tests await the creation of
// a timer with a given duration (which doubles as a synchronization point —
// for example, a buffered call's timeout timer proves the call is buffered)
// and fire timers explicitly. No real time ever passes.
type fakeClock struct {
	mu      sync.Mutex
	timers  []*fakeTimer
	created chan struct{} // closed and replaced each time After adds a timer
}

// fakeTimer is one timer handed out by fakeClock.After.
type fakeTimer struct {
	d       time.Duration
	ch      chan time.Time
	awaited bool // claimed by awaitTimer; guarded by fakeClock.mu
}

func newFakeClock() *fakeClock {
	return &fakeClock{created: make(chan struct{})}
}

// After implements Clock with a timer that fires only when a test fires it.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	timer := &fakeTimer{d: d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	close(c.created)
	c.created = make(chan struct{})
	return timer.ch
}

// awaitTimer blocks until production code has created an unclaimed timer with
// duration d, claims it, and returns it. Claiming means successive awaits for
// the same duration observe distinct timers.
func (c *fakeClock) awaitTimer(t *testing.T, d time.Duration) *fakeTimer {
	t.Helper()

	deadline := time.After(awaitDeadline)
	for {
		c.mu.Lock()
		var found *fakeTimer
		for _, timer := range c.timers {
			if !timer.awaited && timer.d == d {
				found = timer
				break
			}
		}
		if found != nil {
			found.awaited = true
		}
		created := c.created
		c.mu.Unlock()

		if found != nil {
			return found
		}
		select {
		case <-created:
		case <-deadline:
			t.Fatalf("timed out waiting for a %v timer to be created", d)
		}
	}
}

// timerCount reports how many timers with duration d have been created so
// far, claimed or not. Orchestrator tests use it to assert that no retry was
// scheduled (no backoff-duration timer exists).
func (c *fakeClock) timerCount(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for _, timer := range c.timers {
		if timer.d == d {
			count++
		}
	}
	return count
}

// fire delivers the timer's tick.
func (timer *fakeTimer) fire() {
	timer.ch <- time.Now()
}
