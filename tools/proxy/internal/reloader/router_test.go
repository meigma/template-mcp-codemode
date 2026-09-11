package reloader

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testBufferLimit   = 4
	testBufferTimeout = 10 * time.Second
)

func TestRouterCallForwarding(t *testing.T) {
	t.Parallel()

	tc := newRouterContext(t)
	child := NewMockChildSession(t)
	tc.serveChild(child, map[string]string{"search": "fp-1"})

	arguments := json.RawMessage(`{"query":"hello","limit":3}`)
	params := &mcp.CallToolParams{
		Meta:      mcp.Meta{"progressToken": "tok-1", "trace": "abc"},
		Name:      "search",
		Arguments: arguments,
	}
	want := textResult("ok")

	var forwarded *mcp.CallToolParams
	child.EXPECT().CallTool(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, got *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			forwarded = got
			return want, nil
		}).Once()

	result, err := tc.router.CallTool(t.Context(), params)

	require.NoError(t, err)
	assert.Same(t, want, result, "expected the child's result passed through untouched")
	require.NotNil(t, forwarded)
	assert.NotSame(t, params, forwarded, "expected the router to construct fresh CallToolParams")
	assert.Equal(t, "search", forwarded.Name, "expected the tool name preserved")
	assert.Equal(t, arguments, forwarded.Arguments, "expected the raw argument bytes forwarded byte-for-byte")
	assert.Nil(t, forwarded.Meta, "expected Meta, including the progress token, dropped")
}

func TestRouterCallContextCancellation(t *testing.T) {
	t.Parallel()

	tc := newRouterContext(t)
	child := NewMockChildSession(t)
	tc.serveChild(child, map[string]string{"fetch": "fp-1"})

	started := make(chan struct{}, 1)
	child.EXPECT().CallTool(mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}).Once()

	ctx, cancel := context.WithCancel(t.Context())
	results := tc.startCall(ctx, callParams("fetch"))
	awaitSignal(t, started, "timed out waiting for the call to reach the child")

	cancel()

	res := awaitResult(t, results)
	require.ErrorIs(t, res.err, context.Canceled, "expected caller cancellation to propagate to the child call")
	assert.Nil(t, res.result)
}

func TestRouterBufferedCallCancellation(t *testing.T) {
	t.Parallel()

	t.Run("returns the cancellation and never dispatches the parked call", func(t *testing.T) {
		t.Parallel()

		const tool = "search"
		fingerprints := map[string]string{tool: "fp-1"}
		tc := newRouterContext(t)
		tc.serveChild(NewMockChildSession(t), fingerprints)
		tc.router.Quiesce()

		ctx, cancel := context.WithCancel(t.Context())
		results := tc.startCall(ctx, callParams(tool))
		tc.clock.awaitTimer(t, testBufferTimeout) // the call is parked

		cancel()

		res := awaitResult(t, results)
		require.ErrorIs(t, res.err, context.Canceled,
			"expected caller cancellation to resolve the parked call with the context error")
		assert.Nil(t, res.result)

		// The cancellation claimed the call out of the buffer: the drain must
		// not deliver it to the new child, whose mock would fail the test on
		// any unexpected CallTool.
		tc.router.Swap(NewMockChildSession(t))
		tc.router.Drain(t.Context(), fingerprints)
	})

	t.Run("resolves a cancel-versus-drain race exactly once", func(t *testing.T) {
		t.Parallel()

		// bufferedCall documents an exactly-once race: whichever of Drain or
		// caller cancellation claims a parked call first decides it. Racing
		// the two must always yield exactly one of the two outcomes — and a
		// drain decision that wins must be honored, because Drain already
		// charged the call against the in-flight count: abandoning it would
		// leak the charge and hang the post-drain quiesce forever.
		const tool = "search"
		fingerprints := map[string]string{tool: "fp-1"}
		tc := newRouterContext(t)
		tc.serveChild(NewMockChildSession(t), fingerprints)

		child := NewMockChildSession(t)
		want := textResult("served by the new child")
		child.EXPECT().CallTool(mock.Anything, mock.Anything).Return(want, nil).Maybe()

		for range 100 {
			tc.router.Quiesce()
			ctx, cancel := context.WithCancel(t.Context())
			results := tc.startCall(ctx, callParams(tool))
			tc.clock.awaitTimer(t, testBufferTimeout) // the call is parked

			var race sync.WaitGroup
			race.Add(2)
			go func() {
				defer race.Done()
				cancel()
			}()
			go func() {
				defer race.Done()
				tc.router.Swap(child)
				tc.router.Drain(t.Context(), fingerprints)
			}()
			race.Wait()

			res := awaitResult(t, results)
			if res.err != nil {
				require.ErrorIs(t, res.err, context.Canceled,
					"expected a cancellation-claimed call to fail with the context error")
				assert.Nil(t, res.result)
			} else {
				assert.Same(t, want, res.result,
					"expected a drain-claimed call dispatched to the new child")
			}
			awaitSignal(t, tc.router.Quiesce(),
				"timed out waiting for quiesce to drain: the race leaked an in-flight charge")
		}
	})
}

func TestRouterDrainGating(t *testing.T) {
	t.Parallel()

	const tool = "search"
	tests := []struct {
		name      string
		oldFPs    map[string]string
		newFPs    map[string]string
		wantStale bool
	}{
		{
			name:   "forwards a buffered call when the tool definition is unchanged",
			oldFPs: map[string]string{tool: "fp-1"},
			newFPs: map[string]string{tool: "fp-1"},
		},
		{
			name:      "answers stale when the tool definition changed",
			oldFPs:    map[string]string{tool: "fp-1"},
			newFPs:    map[string]string{tool: "fp-2"},
			wantStale: true,
		},
		{
			name:      "answers stale when the tool was removed",
			oldFPs:    map[string]string{tool: "fp-1"},
			newFPs:    map[string]string{},
			wantStale: true,
		},
		{
			name:      "answers stale when the ingress fingerprint is unknown",
			oldFPs:    map[string]string{},
			newFPs:    map[string]string{tool: "fp-1"},
			wantStale: true,
		},
		{
			name:      "answers stale when the fingerprint is an error marker, even if identical",
			oldFPs:    map[string]string{tool: fingerprintErrorMarker + "boom"},
			newFPs:    map[string]string{tool: fingerprintErrorMarker + "boom"},
			wantStale: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := newRouterContext(t)
			oldChild := NewMockChildSession(t)
			tc.serveChild(oldChild, tt.oldFPs)
			tc.router.Quiesce()

			results := tc.startCall(t.Context(), callParams(tool))
			// A buffered call's timeout timer proves the call is parked.
			tc.clock.awaitTimer(t, testBufferTimeout)

			newChild := NewMockChildSession(t)
			want := textResult("served by the new child")
			if !tt.wantStale {
				newChild.EXPECT().CallTool(mock.Anything, mock.Anything).Return(want, nil).Once()
			}
			old := tc.router.Swap(newChild)
			assert.Same(t, oldChild, old, "expected Swap to hand back the previous child for closing")
			tc.router.Drain(t.Context(), tt.newFPs)

			res := awaitResult(t, results)
			require.NoError(t, res.err)
			if tt.wantStale {
				assert.Equal(t, StaleReloadResult(tool), res.result, "expected the friendly stale-reload result")
			} else {
				assert.Same(t, want, res.result, "expected the buffered call served by the new child")
			}
		})
	}
}

func TestRouterBufferLimits(t *testing.T) {
	t.Parallel()

	t.Run("errors excess calls beyond the buffer limit", func(t *testing.T) {
		t.Parallel()

		// The router starts quiesced with no child, exactly the cold-start
		// window: calls buffer until the first swap.
		tc := newRouterContext(t)
		buffered := make([]<-chan callResult, 0, testBufferLimit)
		for range testBufferLimit {
			buffered = append(buffered, tc.startCall(t.Context(), callParams("burst")))
		}
		for range testBufferLimit {
			tc.clock.awaitTimer(t, testBufferTimeout)
		}

		result, err := tc.router.CallTool(t.Context(), callParams("burst"))

		require.NoError(t, err)
		assert.Equal(t, bufferOverflowResult("burst"), result,
			"expected the excess call answered with the overflow result, not queued or blocked")

		tc.router.Close()
		for _, results := range buffered {
			res := awaitResult(t, results)
			require.ErrorIs(t, res.err, errShuttingDown, "expected buffered calls failed at shutdown")
		}
	})

	t.Run("times out buffered calls when the swap stalls", func(t *testing.T) {
		t.Parallel()

		tc := newRouterContext(t)
		results := tc.startCall(t.Context(), callParams("stalled"))
		timer := tc.clock.awaitTimer(t, testBufferTimeout)

		timer.fire()

		res := awaitResult(t, results)
		require.NoError(t, res.err)
		assert.Equal(t, bufferTimeoutResult("stalled"), res.result,
			"expected the stalled buffered call answered with the timeout result")
	})
}

func TestRouterQuiesceDrainSignal(t *testing.T) {
	t.Parallel()

	t.Run("returns a closed channel when nothing is in flight", func(t *testing.T) {
		t.Parallel()

		tc := newRouterContext(t)
		tc.serveChild(NewMockChildSession(t), nil)

		drained := tc.router.Quiesce()

		awaitSignal(t, drained, "expected an immediately closed drain channel with no calls in flight")
	})

	t.Run("closes the channel once in-flight calls complete", func(t *testing.T) {
		t.Parallel()

		tc := newRouterContext(t)
		child := NewMockChildSession(t)
		tc.serveChild(child, map[string]string{"slow": "fp-1"})

		release := make(chan struct{})
		want := textResult("done")
		started := expectBlockedCall(child, release, want, nil)
		results := tc.startCall(t.Context(), callParams("slow"))
		awaitSignal(t, started, "timed out waiting for the call to reach the child")

		drained := tc.router.Quiesce()
		assertStillOpen(t, drained, "expected the drain channel to stay open while a call is in flight")

		close(release)

		awaitSignal(t, drained, "timed out waiting for the drain channel to close")
		res := awaitResult(t, results)
		require.NoError(t, res.err)
		assert.Same(t, want, res.result)
	})
}

func TestRouterSupersededCallErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		childResult *mcp.CallToolResult
		childErr    error
	}{
		{
			name:     "rewrites an error from a superseded child to the interrupted result",
			childErr: errors.New("child terminated"),
		},
		{
			name:        "rewrites a late success from a superseded child to the interrupted result",
			childResult: textResult("late but complete"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := newRouterContext(t)
			fingerprints := map[string]string{"slow-op": "fp-1"}
			oldChild := NewMockChildSession(t)
			tc.serveChild(oldChild, fingerprints)

			release := make(chan struct{})
			started := expectBlockedCall(oldChild, release, tt.childResult, tt.childErr)
			results := tc.startCall(t.Context(), callParams("slow-op"))
			awaitSignal(t, started, "timed out waiting for the call to reach the old child")

			drained := tc.router.Quiesce()
			assertStillOpen(t, drained, "expected quiesce to keep waiting on the in-flight call")

			// The orchestration loop swaps once the grace timeout expires;
			// the old child's in-flight call is now superseded.
			tc.router.Swap(NewMockChildSession(t))
			tc.router.Drain(t.Context(), fingerprints)

			close(release)

			res := awaitResult(t, results)
			require.NoError(t, res.err, "expected the superseded completion rewritten, not surfaced as a Go error")
			assert.Equal(t, supersededResult("slow-op"), res.result,
				"expected any completion on a superseded child answered with the interrupted result")
		})
	}
}

func TestRouterCallsBufferedBetweenSwapAndDrain(t *testing.T) {
	t.Parallel()

	// A call landing after Swap but before Drain was issued against the old
	// generation's still-advertised definitions: ingress must record the old
	// fingerprint, so the drain gate forwards only when the swap kept the
	// definition identical and never silently runs a changed tool on new code.
	const tool = "search"
	tests := []struct {
		name      string
		drainFPs  map[string]string
		wantStale bool
	}{
		{
			name:     "forwards when the swap kept the definition identical",
			drainFPs: map[string]string{tool: "fp-old"},
		},
		{
			name:      "answers stale when the swap changed the definition",
			drainFPs:  map[string]string{tool: "fp-new"},
			wantStale: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := newRouterContext(t)
			tc.serveChild(NewMockChildSession(t), map[string]string{tool: "fp-old"})
			tc.router.Quiesce()

			newChild := NewMockChildSession(t)
			want := textResult("served by the new child")
			if !tt.wantStale {
				newChild.EXPECT().CallTool(mock.Anything, mock.Anything).Return(want, nil).Once()
			}
			tc.router.Swap(newChild)

			results := tc.startCall(t.Context(), callParams(tool))
			tc.clock.awaitTimer(t, testBufferTimeout)

			tc.router.Drain(t.Context(), tt.drainFPs)

			res := awaitResult(t, results)
			require.NoError(t, res.err)
			if tt.wantStale {
				assert.Equal(t, StaleReloadResult(tool), res.result,
					"expected the mid-swap call gated stale against its ingress definition")
			} else {
				assert.Same(t, want, res.result, "expected the mid-swap call drained to the new child")
			}
		})
	}
}

func TestRouterClose(t *testing.T) {
	t.Parallel()

	tc := newRouterContext(t)
	results := tc.startCall(t.Context(), callParams("late"))
	tc.clock.awaitTimer(t, testBufferTimeout)

	tc.router.Close()

	res := awaitResult(t, results)
	require.ErrorIs(t, res.err, errShuttingDown, "expected the buffered call failed with the shutdown error")
	assert.Nil(t, res.result)

	_, err := tc.router.CallTool(t.Context(), callParams("late"))
	require.ErrorIs(t, err, errShuttingDown, "expected new calls rejected after Close")
}

// routerContext carries the shared fixtures for router tests: the fake clock
// and the router under test, which starts in its initial quiesced state.
type routerContext struct {
	clock  *fakeClock
	router *router
}

func newRouterContext(t *testing.T) *routerContext {
	t.Helper()

	clock := newFakeClock()
	return &routerContext{
		clock:  clock,
		router: newRouter(slog.New(slog.DiscardHandler), clock, testBufferLimit, testBufferTimeout),
	}
}

// serveChild installs child as the serving session through the normal
// quiesce, swap, drain sequence.
func (tc *routerContext) serveChild(child ChildSession, fingerprints map[string]string) {
	tc.router.Quiesce()
	tc.router.Swap(child)
	tc.router.Drain(context.Background(), fingerprints)
}

// startCall issues CallTool on its own goroutine and returns the channel its
// outcome lands on.
func (tc *routerContext) startCall(ctx context.Context, params *mcp.CallToolParams) <-chan callResult {
	results := make(chan callResult, 1)
	go func() {
		result, err := tc.router.CallTool(ctx, params)
		results <- callResult{result: result, err: err}
	}()
	return results
}

// callResult is one CallTool outcome funneled out of a test goroutine.
type callResult struct {
	result *mcp.CallToolResult
	err    error
}

// expectBlockedCall arms child.CallTool to block until release is closed and
// then return result and err. The returned channel signals once the call is
// in flight on the child.
func expectBlockedCall(
	child *MockChildSession,
	release <-chan struct{},
	result *mcp.CallToolResult,
	err error,
) <-chan struct{} {
	started := make(chan struct{}, 1)
	child.EXPECT().CallTool(mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			started <- struct{}{}
			<-release
			return result, err
		}).Once()
	return started
}

func awaitResult(t *testing.T, results <-chan callResult) callResult {
	t.Helper()

	select {
	case res := <-results:
		return res
	case <-time.After(awaitDeadline):
		t.Fatal("timed out waiting for CallTool to return")
		return callResult{}
	}
}

// awaitSignal waits for ch to deliver or close, failing the test after the
// await deadline.
func awaitSignal(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(awaitDeadline):
		t.Fatal(message)
	}
}

// assertStillOpen asserts ch has not delivered or closed yet.
func assertStillOpen(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-ch:
		t.Fatal(message)
	default:
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func callParams(name string) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(`{"input":"value"}`)}
}
