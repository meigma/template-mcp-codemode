package reloader

import "time"

// Clock is the time source for the core's debounce, quiesce-grace, buffer,
// and backoff timers, kept as an injectable seam so the orchestration logic
// is testable without real sleeps.
//
// It is deliberately just After: the loop resets its debounce by abandonment
// (each change event arms a fresh timer and the previous channel is
// forgotten), so no stoppable timers are needed. With the real clock an
// abandoned timer leaks until it fires — bounded by the debounce duration,
// irrelevant in a dev tool.
type Clock interface {
	// After returns a channel that delivers the current time once d has
	// elapsed.
	After(d time.Duration) <-chan time.Time
}

// systemClock is the real-time Clock selected when Options.Clock is nil.
type systemClock struct{}

func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}
