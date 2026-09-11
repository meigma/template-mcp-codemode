package reloader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options func(t *testing.T) Options
		wantErr string
	}{
		{
			name: "errors when Watcher is missing",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.Watcher = nil
				return opts
			},
			wantErr: "watcher is required",
		},
		{
			name: "errors when Builder is missing",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.Builder = nil
				return opts
			},
			wantErr: "builder is required",
		},
		{
			name: "errors when Upstream is missing",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.Upstream = nil
				return opts
			},
			wantErr: "upstream is required",
		},
		{
			name: "errors when BufferLimit is negative",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.BufferLimit = -1
				return opts
			},
			wantErr: "buffer limit must not be negative",
		},
		{
			name: "errors when BufferTimeout is negative",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.BufferTimeout = -time.Second
				return opts
			},
			wantErr: "buffer timeout must not be negative",
		},
		{
			name: "errors when Debounce is negative",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.Debounce = -time.Second
				return opts
			},
			wantErr: "debounce must not be negative",
		},
		{
			name: "errors when QuiesceGrace is negative",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.QuiesceGrace = -time.Second
				return opts
			},
			wantErr: "quiesce grace must not be negative",
		},
		{
			name: "errors when BackoffFloor is negative",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.BackoffFloor = -time.Second
				return opts
			},
			wantErr: "backoff floor must not be negative",
		},
		{
			name: "errors when BackoffCeiling is negative",
			options: func(t *testing.T) Options {
				t.Helper()
				opts := validOptions(t)
				opts.BackoffCeiling = -time.Second
				return opts
			},
			wantErr: "backoff ceiling must not be negative",
		},
		{
			name:    "constructs from the required ports alone",
			options: validOptions,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := tt.options(t)

			got, err := New(opts)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, got, "expected no Reloader alongside a construction error")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)

			// The nil-Logger and nil-Clock defaults must be usable, not just
			// non-nil: a call issued before any child serves parks in the swap
			// buffer (its timeout armed via the default real-time clock) and
			// resolves through caller cancellation without panicking.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			result, callErr := got.CallTool(ctx, callParams("probe"))
			require.ErrorIs(t, callErr, context.Canceled,
				"expected the cancelled pre-first-swap call to surface the context error")
			assert.Nil(t, result)
		})
	}
}

// TestNewDefaultKnobs proves that zero-valued Options select the documented
// non-zero defaults, observed behaviorally at the Clock seam: each timing
// knob surfaces as the duration of a timer the core requests, and the buffer
// limit surfaces as the buffered call being admitted rather than rejected.
func TestNewDefaultKnobs(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	watcher := NewMockWatcher(t)
	builder := NewMockBuilder(t)
	events := make(chan ChangeEvent)
	watcher.EXPECT().Watch(mock.Anything).Return((<-chan ChangeEvent)(events), nil).Once()
	buildFailed := make(chan struct{}, 1)
	builder.EXPECT().Build(mock.Anything).RunAndReturn(
		func(context.Context) (BuildResult, error) {
			buildFailed <- struct{}{}
			return BuildResult{}, errors.New("compile error: main.go:1")
		}).Once()

	r, err := New(Options{
		Watcher:  watcher,
		Builder:  builder,
		Upstream: NewMockUpstream(t),
		Clock:    clock,
	})
	require.NoError(t, err)
	r.SetFrontend(NewMockFrontend(t))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// The failed first build schedules its retry at the default backoff floor.
	awaitSignal(t, buildFailed, "timed out waiting for the cold-start build attempt")
	clock.awaitTimer(t, defaultBackoffFloor)

	// A change event is coalesced under the default debounce.
	select {
	case events <- ChangeEvent{Path: "main.go"}:
	case <-time.After(awaitDeadline):
		t.Fatal("timed out delivering a change event to the loop")
	}
	clock.awaitTimer(t, defaultDebounce)

	// With no child serving yet, a call is admitted to the swap buffer (the
	// default limit is non-zero) under the default per-call timeout.
	results := make(chan callResult, 1)
	go func() {
		result, callErr := r.CallTool(t.Context(), callParams("probe"))
		results <- callResult{result: result, err: callErr}
	}()
	clock.awaitTimer(t, defaultBufferTimeout).fire()
	res := awaitResult(t, results)
	require.NoError(t, res.err)
	assert.Equal(t, bufferTimeoutResult("probe"), res.result,
		"expected the buffered call answered with the timeout result once the default buffer timeout fired")

	cancel()
	select {
	case runErr := <-done:
		require.NoError(t, runErr, "expected Run to return nil on shutdown")
	case <-time.After(awaitDeadline):
		t.Fatal("timed out waiting for Run to return")
	}
}

func validOptions(t *testing.T) Options {
	t.Helper()

	return Options{
		Watcher:  NewMockWatcher(t),
		Builder:  NewMockBuilder(t),
		Upstream: NewMockUpstream(t),
	}
}
