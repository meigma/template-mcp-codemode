package reloader

import (
	"context"
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

// Orchestrator test knobs. Every duration is distinct so fakeClock.awaitTimer
// can identify a timer by duration alone: the backoff sequence (250ms, 500ms,
// 1s, ...) never collides with debounce, grace, or the buffer timeout.
const (
	runDebounce       = 300 * time.Millisecond
	runQuiesceGrace   = 7 * time.Second
	runBackoffFloor   = 250 * time.Millisecond
	runBackoffCeiling = 8 * time.Second
	runBufferTimeout  = 30 * time.Second

	burstSize = 5

	// coldStartArtifact is the artifact every coldStart cycle builds; crash
	// tests assert build-free restarts re-Start exactly this path.
	coldStartArtifact = "art1"
)

func TestRunPreconditions(t *testing.T) {
	t.Parallel()

	t.Run("errors when no frontend is set", func(t *testing.T) {
		t.Parallel()

		r, err := New(validOptions(t))
		require.NoError(t, err)

		err = r.Run(t.Context())

		require.ErrorContains(t, err, "frontend is required: call SetFrontend before Run")
	})

	t.Run("wraps a watcher failure", func(t *testing.T) {
		t.Parallel()

		opts := validOptions(t)
		watcher, ok := opts.Watcher.(*MockWatcher)
		require.True(t, ok)
		watcher.EXPECT().Watch(mock.Anything).Return(nil, errors.New("fsnotify exploded")).Once()
		r, err := New(opts)
		require.NoError(t, err)
		r.SetFrontend(NewMockFrontend(t))

		err = r.Run(t.Context())

		require.ErrorContains(t, err, "watch source tree")
		require.ErrorContains(t, err, "fsnotify exploded")
	})

	t.Run("returns an error when the watcher stream closes unexpectedly", func(t *testing.T) {
		t.Parallel()

		tc := newRunContext(t)
		tc.wantRunErr = true
		child := tc.coldStart(t, toolSet("search", "v1"))
		child.expectClose()

		close(tc.events)

		require.ErrorContains(t, tc.waitRun(t), "watcher channel closed unexpectedly",
			"expected a dead watcher to fail Run loudly")
	})
}

func TestRunColdStart(t *testing.T) {
	t.Parallel()

	t.Run("builds immediately and reconciles the first healthy child", func(t *testing.T) {
		t.Parallel()

		tc := newRunContext(t)
		toolsV1 := toolSet("search", "v1")
		child := tc.newChild(t, toolsV1)
		tc.expectBuild(coldStartArtifact)
		tc.expectStart(coldStartArtifact, child)
		reconciled := tc.expectReconcile(toolsV1, nil)
		child.expectClose()

		tc.start(t)

		awaitSignal(t, reconciled, "timed out waiting for the cold-start reconcile")
		tc.assertServedBy(t, child, "search")
	})

	t.Run("retries a failed first build with backoff", func(t *testing.T) {
		t.Parallel()

		tc := newRunContext(t)
		buildFailed := tc.expectBuildFailure(errors.New("compile error: main.go:1"))
		tc.start(t)
		awaitSignal(t, buildFailed, "timed out waiting for the first build attempt")

		toolsV1 := toolSet("search", "v1")
		child := tc.newChild(t, toolsV1)
		tc.expectBuild(coldStartArtifact)
		tc.expectStart(coldStartArtifact, child)
		reconciled := tc.expectReconcile(toolsV1, nil)
		child.expectClose()

		tc.clock.awaitTimer(t, runBackoffFloor).fire()

		awaitSignal(t, reconciled, "timed out waiting for the retried cold start to reconcile")
		tc.assertServedBy(t, child, "search")
	})
}

func TestRunCycleFailureKeepsOldChild(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// armFailure arms one failing cycle and returns the channel that
		// signals when the failure happened.
		armFailure func(tc *runContext) <-chan struct{}
		// recoveryArtifact is what the recovery cycle's build produces; the
		// health-gate case consumed "art2" on its failed cycle.
		recoveryArtifact string
	}{
		{
			name: "build failure keeps the old child serving",
			armFailure: func(tc *runContext) <-chan struct{} {
				return tc.expectBuildFailure(errors.New("compile error: syntax"))
			},
			recoveryArtifact: "art2",
		},
		{
			name: "health-gate failure keeps the old child serving",
			armFailure: func(tc *runContext) <-chan struct{} {
				tc.expectBuild("art2")
				return tc.expectStartFailure("art2", errors.New("health gate: duplicate tool names"))
			},
			recoveryArtifact: "art3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := newRunContext(t)
			childA := tc.coldStart(t, toolSet("search", "v1"))

			failed := tt.armFailure(tc)
			tc.triggerChangeAndDebounce(t)
			awaitSignal(t, failed, "timed out waiting for the cycle to fail")

			tc.assertServedBy(t, childA, "search")

			// The next save retriggers and recovers without any backoff
			// machinery.
			toolsV2 := toolSet("search", "v2")
			childB := tc.newChild(t, toolsV2)
			tc.expectBuild(tt.recoveryArtifact)
			tc.expectStart(tt.recoveryArtifact, childB)
			reconciled := tc.expectReconcile(toolsV2, nil)
			childA.expectClose()
			childB.expectClose()
			tc.triggerChangeAndDebounce(t)

			awaitSignal(t, reconciled, "timed out waiting for the recovery cycle to reconcile")
			assert.Zero(t, tc.clock.timerCount(runBackoffFloor),
				"expected no backoff retry scheduled while a healthy child serves")
		})
	}
}

func TestRunSuccessfulCycleSwaps(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	toolsV1 := toolSet("search", "v1")
	childA := tc.coldStart(t, toolsV1)

	var (
		orderMu sync.Mutex
		order   []string
	)
	record := func(event string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, event)
	}

	// Hold one call in flight on the old child so the swap must quiesce.
	release := make(chan struct{})
	heldWant := textResult("old child result")
	started := expectBlockedCall(childA.mock, release, heldWant, nil)
	held := tc.startCall(t.Context(), callParams("search"))
	awaitSignal(t, started, "timed out waiting for the held call to reach the old child")

	// The new child keeps "search" identical (so the buffered call drains to
	// it) and adds a tool (so the set differs and Reconcile must run).
	toolsV2 := append(toolSet("search", "v1"), &mcp.Tool{Name: "extra", Description: "v1"})
	childB := tc.newChild(t, toolsV2)
	tc.expectBuild("art2")
	tc.expectStart("art2", childB)
	childA.mock.EXPECT().Close().RunAndReturn(func() error {
		record("close-old")
		return nil
	}).Once()
	reconciled := make(chan struct{}, 1)
	tc.frontend.EXPECT().Reconcile(toolsV2, mock.Anything).RunAndReturn(
		func([]*mcp.Tool, CallToolFunc) error {
			record("reconcile")
			reconciled <- struct{}{}
			return nil
		}).Once()
	bufferedWant := textResult("new child result")
	childB.mock.EXPECT().CallTool(mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			record("serve-buffered")
			return bufferedWant, nil
		}).Once()
	childB.expectClose()

	tc.triggerChangeAndDebounce(t)
	tc.clock.awaitTimer(t, runQuiesceGrace) // the swap is waiting on the held call

	buffered := tc.startCall(t.Context(), callParams("search"))
	tc.clock.awaitTimer(t, runBufferTimeout) // the mid-swap call is parked

	close(release) // the held call drains; the swap proceeds

	heldRes := awaitResult(t, held)
	require.NoError(t, heldRes.err)
	assert.Same(t, heldWant, heldRes.result, "expected the drained in-flight call answered by the old child")

	awaitSignal(t, reconciled, "timed out waiting for the swap to reconcile")
	bufRes := awaitResult(t, buffered)
	require.NoError(t, bufRes.err)
	assert.Same(t, bufferedWant, bufRes.result, "expected the buffered call drained to the new child")

	orderMu.Lock()
	defer orderMu.Unlock()
	assert.Equal(t, []string{"close-old", "reconcile", "serve-buffered"}, order,
		"expected the swap to close the old child, then reconcile, then drain the buffer")
}

func TestRunIdenticalToolSetSkipsReconcile(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))

	// The crash restart serves byte-identical definitions: no Reconcile (and
	// no Build) is armed, so either call would fail the test as unexpected.
	childB := tc.newChild(t, toolSet("search", "v1"))
	started := tc.expectStartSignaled(coldStartArtifact, childB)
	childA.expectClose()
	childB.expectClose()

	childA.crash()

	awaitSignal(t, started, "timed out waiting for the immediate crash restart")
	tc.assertServedBy(t, childB, "search")
}

func TestRunCancelAndSupersede(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))

	// The first cycle's Start returns a (stale) child only once its ctx is
	// cancelled: if the superseding change failed to cancel the cycle, the
	// test would hang and time out.
	stale := tc.newChild(t, toolSet("search", "stale"))
	tc.expectBuild("art2")
	startBegan := make(chan struct{}, 1)
	tc.upstream.EXPECT().Start(mock.Anything, "art2").RunAndReturn(
		func(ctx context.Context, _ string) (ChildSession, error) {
			startBegan <- struct{}{}
			<-ctx.Done()
			return stale.mock, nil
		}).Once()
	stale.expectClose()

	toolsV2 := toolSet("search", "v2")
	childB := tc.newChild(t, toolsV2)
	tc.expectBuild("art3")
	tc.expectStart("art3", childB)
	reconciled := tc.expectReconcile(toolsV2, nil)
	childA.expectClose()
	childB.expectClose()

	tc.triggerChangeAndDebounce(t)
	awaitSignal(t, startBegan, "timed out waiting for the first cycle to reach Start")
	tc.triggerChangeAndDebounce(t) // mid-cycle change: cancel-and-supersede

	awaitSignal(t, reconciled, "timed out waiting for the single final cycle to reconcile")
	tc.assertServedBy(t, childB, "search")
}

func TestRunDebounceCoalescesBursts(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))

	toolsV2 := toolSet("search", "v2")
	childB := tc.newChild(t, toolsV2)
	tc.expectBuild("art2") // exactly one build for the whole burst
	tc.expectStart("art2", childB)
	reconciled := tc.expectReconcile(toolsV2, nil)
	childA.expectClose()
	childB.expectClose()

	timers := make([]*fakeTimer, 0, burstSize)
	for range burstSize {
		tc.sendChange(t)
		timers = append(timers, tc.clock.awaitTimer(t, runDebounce))
	}
	// Each event abandoned the previous debounce timer; only the last one is
	// still live.
	timers[len(timers)-1].fire()

	awaitSignal(t, reconciled, "timed out waiting for the coalesced cycle to reconcile")
	tc.assertServedBy(t, childB, "search")
}

func TestRunCrashRestartsWithBackoff(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))

	// No Build is armed until the final fresh save: every crash restart must
	// reuse the last good artifact. The first attempt is immediate — no timer
	// — and it plus the first backoff retry fail before the doubled retry
	// succeeds.
	tc.upstream.EXPECT().Start(mock.Anything, coldStartArtifact).Return(nil, errors.New("spawn failed")).Twice()
	toolsV2 := toolSet("search", "v2")
	childB := tc.newChild(t, toolsV2)
	tc.expectStart(coldStartArtifact, childB)
	reconciledB := tc.expectReconcile(toolsV2, nil)
	childA.expectClose()

	childA.crash()                                   // the immediate restart attempt fails
	tc.clock.awaitTimer(t, runBackoffFloor).fire()   // first retry, at the floor, fails
	tc.clock.awaitTimer(t, 2*runBackoffFloor).fire() // doubled retry succeeds

	awaitSignal(t, reconciledB, "timed out waiting for the restarted child to reconcile")
	tc.assertServedBy(t, childB, "search")

	// childB came from a build-free restart, so its success advanced the
	// backoff instead of clearing it: when childB itself crashes — the classic
	// crash loop, where every restart health-gates before dying — the next
	// restart is scheduled at the doubled delay, never run immediately. No
	// immediate Start is armed: one would fail the test as an unexpected call.
	childB.crash()
	retry := tc.clock.awaitTimer(t, 4*runBackoffFloor)

	toolsV3 := toolSet("search", "v3")
	childC := tc.newChild(t, toolsV3)
	tc.expectStart(coldStartArtifact, childC)
	reconciledC := tc.expectReconcile(toolsV3, nil)
	childB.expectClose()
	retry.fire()
	awaitSignal(t, reconciledC, "timed out waiting for the backed-off crash restart to reconcile")

	// A fresh debounced build clears the backoff: the developer changed the
	// code, which is the only thing that can fix a crash loop.
	toolsV4 := toolSet("search", "v4")
	childD := tc.newChild(t, toolsV4)
	tc.expectBuild("art2")
	tc.expectStart("art2", childD)
	reconciledD := tc.expectReconcile(toolsV4, nil)
	childC.expectClose()
	tc.triggerChangeAndDebounce(t)
	awaitSignal(t, reconciledD, "timed out waiting for the fresh build to reconcile")

	// With the backoff cleared, the next crash restarts immediately again.
	toolsV5 := toolSet("search", "v5")
	childE := tc.newChild(t, toolsV5)
	started := tc.expectStartSignaled("art2", childE)
	reconciledE := tc.expectReconcile(toolsV5, nil)
	childD.expectClose()
	childE.expectClose()
	childD.crash()
	awaitSignal(t, started, "timed out waiting for the immediate restart after a fresh build")
	awaitSignal(t, reconciledE, "timed out waiting for the post-reset crash restart to reconcile")
	tc.assertServedBy(t, childE, "search")
}

func TestRunDebouncedCycleDropsPendingRetry(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)

	// The cold-start cycle blocks in Build so a source change can land while
	// it is in flight, then fails with no healthy child to keep: a backoff
	// retry is armed while the change's debounce is still pending.
	releaseBuild := make(chan struct{})
	buildBegan := make(chan struct{}, 1)
	tc.builder.EXPECT().Build(mock.Anything).RunAndReturn(
		func(context.Context) (BuildResult, error) {
			buildBegan <- struct{}{}
			<-releaseBuild
			return BuildResult{}, errors.New("compile error: main.go:1")
		}).Once()
	tc.start(t)
	awaitSignal(t, buildBegan, "timed out waiting for the first build attempt")

	tc.sendChange(t)
	debounce := tc.clock.awaitTimer(t, runDebounce)
	close(releaseBuild)
	retry := tc.clock.awaitTimer(t, runBackoffFloor) // the failure armed a retry

	// The debounce starts the fresh full-build cycle, dropping the pending
	// retry; the cycle blocks in Start to hold the in-flight window open.
	toolsV1 := toolSet("search", "v1")
	child := tc.newChild(t, toolsV1)
	tc.expectBuild("art2")
	releaseStart := make(chan struct{})
	startBegan := tc.expectBlockedStart("art2", child, releaseStart)
	reconciled := tc.expectReconcile(toolsV1, nil)
	child.expectClose()
	debounce.fire()
	awaitSignal(t, startBegan, "timed out waiting for the debounced cycle to reach Start")

	// The stale retry firing mid-cycle must not start a second cycle racing
	// the first for the downstream session: a second cycle would run an
	// unexpected extra Build and orphan one of the two children.
	retry.fire()
	close(releaseStart)

	awaitSignal(t, reconciled, "timed out waiting for the single fresh cycle to reconcile")
	tc.assertServedBy(t, child, "search")
}

func TestRunCrashMidCycleAbsorbed(t *testing.T) {
	t.Parallel()

	t.Run("a successful in-flight cycle replaces the crashed child", func(t *testing.T) {
		t.Parallel()

		tc := newRunContext(t)
		childA := tc.coldStart(t, toolSet("search", "v1"))

		toolsV2 := toolSet("search", "v2")
		childB := tc.newChild(t, toolsV2)
		tc.expectBuild("art2")
		release := make(chan struct{})
		startBegan := tc.expectBlockedStart("art2", childB, release)
		reconciled := tc.expectReconcile(toolsV2, nil)
		childA.expectClose()
		childB.expectClose()

		tc.triggerChangeAndDebounce(t)
		awaitSignal(t, startBegan, "timed out waiting for the cycle to reach Start")

		childA.crash() // mid-cycle: noted, no restart machinery
		close(release) // the cycle completes and swaps

		awaitSignal(t, reconciled, "timed out waiting for the in-flight cycle to reconcile")
		tc.assertServedBy(t, childB, "search")
		assert.Zero(t, tc.clock.timerCount(runBackoffFloor),
			"expected no backoff retry for a crash absorbed by the in-flight cycle")
	})

	t.Run("calls arriving after a mid-cycle crash buffer until the cycle swaps", func(t *testing.T) {
		t.Parallel()

		// The loop's only observable side effects for a mid-cycle crash are
		// router quiescence and the "noted" log line, which onChildDied emits
		// after quiescing — so the signal proves the crash-window is buffering
		// before the test issues its call.
		crashNoted := make(chan struct{}, 1)
		tc := newRunContextWithLogger(t, slog.New(&logSignaler{
			message: "child died during an in-flight reload cycle; the cycle replaces it",
			signals: crashNoted,
		}))
		childA := tc.coldStart(t, toolSet("search", "v1"))

		// The cycle's child serves a byte-identical definition so the buffered
		// call can drain to it; the identical set means no Reconcile is armed.
		childB := tc.newChild(t, toolSet("search", "v1"))
		tc.expectBuild("art2")
		release := make(chan struct{})
		startBegan := tc.expectBlockedStart("art2", childB, release)
		childA.expectClose()
		childB.expectClose()

		tc.triggerChangeAndDebounce(t)
		awaitSignal(t, startBegan, "timed out waiting for the cycle to reach Start")

		childA.crash()
		awaitSignal(t, crashNoted, "timed out waiting for the loop to note the mid-cycle crash")

		// The crash-window call parks in the swap buffer — the dead child's
		// mock has no CallTool armed, so dispatching to its dead transport
		// would fail the test loudly.
		buffered := tc.startCall(t.Context(), callParams("search"))
		tc.clock.awaitTimer(t, runBufferTimeout) // the call is parked

		want := textResult("served by the cycle's child")
		childB.mock.EXPECT().CallTool(mock.Anything, mock.Anything).Return(want, nil).Once()
		close(release) // the cycle completes and swaps

		bufRes := awaitResult(t, buffered)
		require.NoError(t, bufRes.err)
		assert.Same(t, want, bufRes.result,
			"expected the crash-window call buffered, then drained to the cycle's child")
		assert.Zero(t, tc.clock.timerCount(runBackoffFloor),
			"expected no backoff retry for a crash absorbed by the in-flight cycle")
	})

	t.Run("a failed cycle after a mid-cycle crash buffers calls and schedules a build-free retry", func(t *testing.T) {
		t.Parallel()

		tc := newRunContext(t)
		childA := tc.coldStart(t, toolSet("search", "v1"))

		tc.expectBuild("art2")
		release := make(chan struct{})
		startBegan := make(chan struct{}, 1)
		tc.upstream.EXPECT().Start(mock.Anything, "art2").RunAndReturn(
			func(context.Context, string) (ChildSession, error) {
				startBegan <- struct{}{}
				<-release
				return nil, errors.New("health gate failed")
			}).Once()

		tc.triggerChangeAndDebounce(t)
		awaitSignal(t, startBegan, "timed out waiting for the cycle to reach Start")

		childA.crash()
		close(release) // the cycle now fails with no healthy child left

		// The failure quiesces the router: a retry-window call parks in the
		// swap buffer — the dead child's mock has no CallTool armed, so a
		// dispatch to its dead transport would fail the test loudly.
		retry := tc.clock.awaitTimer(t, runBackoffFloor)
		buffered := tc.startCall(t.Context(), callParams("search"))
		tc.clock.awaitTimer(t, runBufferTimeout) // the call is parked

		toolsV2 := toolSet("search", "v2")
		childB := tc.newChild(t, toolsV2)
		tc.expectStart(coldStartArtifact, childB) // build-free restart of the last artifact
		reconciled := tc.expectReconcile(toolsV2, nil)
		childA.expectClose()
		childB.expectClose()

		retry.fire()

		awaitSignal(t, reconciled, "timed out waiting for the rescue restart to reconcile")
		bufRes := awaitResult(t, buffered)
		require.NoError(t, bufRes.err, "expected the retry-window call buffered, not failed on the dead child")
		assert.Equal(t, StaleReloadResult("search"), bufRes.result,
			"expected the buffered call gated stale: the rescue child changed the definition")
		tc.assertServedBy(t, childB, "search")
	})
}

func TestRunToolsChangedReconciles(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))
	childA.expectClose()

	// No Build or Start is armed past the cold start: a runtime tool change
	// must reconcile without a restart. The first reconcile fails to prove
	// the child keeps serving through a Frontend error.
	toolsV2 := toolSet("search", "v2")
	reconcileFailed := tc.expectReconcile(toolsV2, errors.New("frontend rejected a definition"))
	tc.sendTools(t, childA, toolsV2)
	awaitSignal(t, reconcileFailed, "timed out waiting for the runtime tool-change reconcile")

	// After a failed reconcile the advertised set cannot be trusted, so an
	// identical snapshot must retry instead of being skipped.
	reconcileRetried := tc.expectReconcile(toolsV2, nil)
	tc.sendTools(t, childA, toolSet("search", "v2"))
	awaitSignal(t, reconcileRetried, "timed out waiting for the reconcile retry on the identical snapshot")

	// Once healed, an identical snapshot is skipped outright: no Reconcile is
	// armed for it, and the next distinct snapshot proves the loop consumed
	// it.
	tc.sendTools(t, childA, toolSet("search", "v2"))

	toolsV3 := toolSet("search", "v3")
	reconciledV3 := tc.expectReconcile(toolsV3, nil)
	tc.sendTools(t, childA, toolsV3)
	awaitSignal(t, reconciledV3, "timed out waiting for the next runtime tool-change reconcile")

	tc.assertServedBy(t, childA, "search")
}

func TestRunErrorMarkerFingerprintsSuppressReconcileSkip(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	tools := []*mcp.Tool{unmarshalableToolFixture()}
	childA := tc.coldStart(t, tools)
	childA.expectClose()

	// The runtime snapshot is byte-identical, but its fingerprint is an error
	// marker: the wire form is unknown, so "unchanged" cannot be trusted and
	// the identical-set skip must not apply — the same conservative direction
	// the router's drain gate takes for marker fingerprints.
	reconciled := tc.expectReconcile(tools, nil)
	tc.sendTools(t, childA, tools)
	awaitSignal(t, reconciled, "timed out waiting for the marker-fingerprint snapshot to reconcile")

	tc.assertServedBy(t, childA, "bad")
}

func TestRunFailedReconcileRetriedOnNextCycle(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))

	// The swap to childB serves v2 but its reconcile fails: the advertised
	// set no longer matches the served set.
	toolsV2 := toolSet("search", "v2")
	childB := tc.newChild(t, toolsV2)
	tc.expectBuild("art2")
	tc.expectStart("art2", childB)
	reconcileFailed := tc.expectReconcile(toolsV2, errors.New("frontend rejected a definition"))
	childA.expectClose()
	tc.triggerChangeAndDebounce(t)
	awaitSignal(t, reconcileFailed, "timed out waiting for the failing reconcile")

	// The next cycle serves the fingerprint-identical set: the identical-set
	// skip must not suppress the retry that heals the advertised set.
	childC := tc.newChild(t, toolSet("search", "v2"))
	tc.expectBuild("art3")
	tc.expectStart("art3", childC)
	reconcileRetried := tc.expectReconcile(toolsV2, nil)
	childB.expectClose()
	childC.expectClose()
	tc.triggerChangeAndDebounce(t)

	awaitSignal(t, reconcileRetried, "timed out waiting for the reconcile retry on the identical set")
	tc.assertServedBy(t, childC, "search")
}

func TestRunSwapProceedsAfterGraceTimeout(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("slow", "v1"))

	release := make(chan struct{})
	started := expectBlockedCall(childA.mock, release, nil, errors.New("connection torn down"))
	held := tc.startCall(t.Context(), callParams("slow"))
	awaitSignal(t, started, "timed out waiting for the held call to reach the old child")

	toolsV2 := toolSet("slow", "v2")
	childB := tc.newChild(t, toolsV2)
	tc.expectBuild("art2")
	tc.expectStart("art2", childB)
	reconciled := tc.expectReconcile(toolsV2, nil)
	childA.expectClose()
	childB.expectClose()

	tc.triggerChangeAndDebounce(t)
	tc.clock.awaitTimer(t, runQuiesceGrace).fire() // the held call never drains

	awaitSignal(t, reconciled, "timed out waiting for the post-grace swap to reconcile")

	close(release) // the stranded call finally errors on the superseded child

	heldRes := awaitResult(t, held)
	require.NoError(t, heldRes.err)
	assert.Equal(t, supersededResult("slow"), heldRes.result,
		"expected the call stranded past the grace timeout answered with the interrupted result")
	tc.assertServedBy(t, childB, "slow")
}

func TestRunShutdownMidSwapClosesBoth(t *testing.T) {
	t.Parallel()

	tc := newRunContext(t)
	childA := tc.coldStart(t, toolSet("search", "v1"))

	release := make(chan struct{})
	heldWant := textResult("finished after shutdown")
	started := expectBlockedCall(childA.mock, release, heldWant, nil)
	held := tc.startCall(t.Context(), callParams("search"))
	awaitSignal(t, started, "timed out waiting for the held call to reach the old child")

	// No Reconcile is armed: shutdown lands before the swap gets that far.
	childB := tc.newChild(t, toolSet("search", "v2"))
	tc.expectBuild("art2")
	tc.expectStart("art2", childB)
	childA.expectClose() // still current at shutdown
	childB.expectClose() // the mid-swap candidate must be closed too

	tc.triggerChangeAndDebounce(t)
	tc.clock.awaitTimer(t, runQuiesceGrace) // the swap is blocked on the held call

	buffered := tc.startCall(t.Context(), callParams("search"))
	tc.clock.awaitTimer(t, runBufferTimeout) // the mid-swap call is parked

	tc.cancel() // shutdown mid-swap

	require.NoError(t, tc.waitRun(t), "expected a clean Run return when signaled mid-swap")

	bufRes := awaitResult(t, buffered)
	require.ErrorIs(t, bufRes.err, errShuttingDown, "expected buffered calls failed at shutdown")

	close(release)
	heldRes := awaitResult(t, held)
	require.NoError(t, heldRes.err)
	assert.Same(t, heldWant, heldRes.result, "expected the held call's late completion still delivered")
}

// runContext carries the shared fixtures for orchestrator tests: the five
// port mocks, the fake clock, the test-owned watcher event stream, and the
// Reloader under test.
type runContext struct {
	watcher  *MockWatcher
	builder  *MockBuilder
	upstream *MockUpstream
	frontend *MockFrontend
	clock    *fakeClock
	reloader *Reloader
	events   chan ChangeEvent
	// children collects every child-session mock so start's cleanup can
	// assert their expectations after Run has been joined: t.Cleanup runs
	// LIFO, so mockery's own auto-assert on a child created mid-test would
	// fire before the shutdown path closes the serving child.
	children []*MockChildSession

	cancel context.CancelFunc
	done   chan error
	runErr error
	// finished memoizes the Run result so waitRun is callable from both the
	// test body and the cleanup join.
	finished bool
	// wantRunErr suppresses the cleanup's clean-shutdown assertion for tests
	// that expect Run to return an error.
	wantRunErr bool
}

func newRunContext(t *testing.T) *runContext {
	t.Helper()

	return newRunContextWithLogger(t, nil)
}

// newRunContextWithLogger is newRunContext with an explicit logger, for tests
// that synchronize on the loop's log output. A nil logger selects the no-op
// default.
func newRunContextWithLogger(t *testing.T, logger *slog.Logger) *runContext {
	t.Helper()

	tc := &runContext{
		watcher:  NewMockWatcher(t),
		builder:  NewMockBuilder(t),
		upstream: NewMockUpstream(t),
		frontend: NewMockFrontend(t),
		clock:    newFakeClock(),
		events:   make(chan ChangeEvent),
	}
	tc.watcher.EXPECT().Watch(mock.Anything).Return((<-chan ChangeEvent)(tc.events), nil).Maybe()

	reloader, err := New(Options{
		Watcher:        tc.watcher,
		Builder:        tc.builder,
		Upstream:       tc.upstream,
		Logger:         logger,
		Clock:          tc.clock,
		Debounce:       runDebounce,
		QuiesceGrace:   runQuiesceGrace,
		BackoffFloor:   runBackoffFloor,
		BackoffCeiling: runBackoffCeiling,
		BufferTimeout:  runBufferTimeout,
	})
	require.NoError(t, err)
	reloader.SetFrontend(tc.frontend)
	tc.reloader = reloader
	return tc
}

// start launches Run on its own goroutine and registers a cleanup that
// cancels it and joins, asserting a clean shutdown unless wantRunErr is set.
func (tc *runContext) start(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	tc.cancel = cancel
	tc.done = make(chan error, 1)
	go func() { tc.done <- tc.reloader.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		err := tc.waitRun(t)
		if !tc.wantRunErr {
			assert.NoError(t, err, "expected Run to return nil on shutdown")
		}
		for _, child := range tc.children {
			child.AssertExpectations(t)
		}
	})
}

// waitRun joins the Run goroutine and memoizes its result.
func (tc *runContext) waitRun(t *testing.T) error {
	t.Helper()

	if tc.finished {
		return tc.runErr
	}
	select {
	case tc.runErr = <-tc.done:
		tc.finished = true
	case <-time.After(awaitDeadline):
		t.Fatal("timed out waiting for Run to return")
	}
	return tc.runErr
}

// coldStart arms a full first cycle (build, start, reconcile) producing
// coldStartArtifact, starts Run, and waits until the child serves. The
// child's shutdown-time Close is the test's responsibility.
func (tc *runContext) coldStart(t *testing.T, tools []*mcp.Tool) *testChild {
	t.Helper()

	child := tc.newChild(t, tools)
	tc.expectBuild(coldStartArtifact)
	tc.expectStart(coldStartArtifact, child)
	reconciled := tc.expectReconcile(tools, nil)
	tc.start(t)
	awaitSignal(t, reconciled, "timed out waiting for the cold-start reconcile")
	return child
}

// expectBuild arms one successful build producing artifact.
func (tc *runContext) expectBuild(artifact string) {
	tc.builder.EXPECT().Build(mock.Anything).Return(BuildResult{Artifact: artifact}, nil).Once()
}

// expectBuildFailure arms one failing build; the returned channel signals
// when it was attempted.
func (tc *runContext) expectBuildFailure(err error) <-chan struct{} {
	called := make(chan struct{}, 1)
	tc.builder.EXPECT().Build(mock.Anything).RunAndReturn(
		func(context.Context) (BuildResult, error) {
			called <- struct{}{}
			return BuildResult{}, err
		}).Once()
	return called
}

// expectStart arms one successful Start of artifact yielding child.
func (tc *runContext) expectStart(artifact string, child *testChild) {
	tc.upstream.EXPECT().Start(mock.Anything, artifact).Return(child.mock, nil).Once()
}

// expectStartSignaled arms one successful Start of artifact yielding child;
// the returned channel signals when it ran. Crash tests await it because an
// immediate restart creates no timer to synchronize on.
func (tc *runContext) expectStartSignaled(artifact string, child *testChild) <-chan struct{} {
	started := make(chan struct{}, 1)
	tc.upstream.EXPECT().Start(mock.Anything, artifact).RunAndReturn(
		func(context.Context, string) (ChildSession, error) {
			started <- struct{}{}
			return child.mock, nil
		}).Once()
	return started
}

// expectStartFailure arms one failing Start; the returned channel signals
// when it was attempted.
func (tc *runContext) expectStartFailure(artifact string, err error) <-chan struct{} {
	called := make(chan struct{}, 1)
	tc.upstream.EXPECT().Start(mock.Anything, artifact).RunAndReturn(
		func(context.Context, string) (ChildSession, error) {
			called <- struct{}{}
			return nil, err
		}).Once()
	return called
}

// expectBlockedStart arms one Start of artifact that blocks until release is
// closed, then yields child. The returned channel signals once Start is in
// flight.
func (tc *runContext) expectBlockedStart(artifact string, child *testChild, release <-chan struct{}) <-chan struct{} {
	started := make(chan struct{}, 1)
	tc.upstream.EXPECT().Start(mock.Anything, artifact).RunAndReturn(
		func(context.Context, string) (ChildSession, error) {
			started <- struct{}{}
			<-release
			return child.mock, nil
		}).Once()
	return started
}

// expectReconcile arms one Reconcile for exactly tools, returning err to the
// loop; the returned channel signals when it ran.
func (tc *runContext) expectReconcile(tools []*mcp.Tool, err error) <-chan struct{} {
	called := make(chan struct{}, 1)
	tc.frontend.EXPECT().Reconcile(tools, mock.Anything).RunAndReturn(
		func([]*mcp.Tool, CallToolFunc) error {
			called <- struct{}{}
			return err
		}).Once()
	return called
}

// startCall issues CallTool through the Reloader on its own goroutine.
func (tc *runContext) startCall(ctx context.Context, params *mcp.CallToolParams) <-chan callResult {
	results := make(chan callResult, 1)
	go func() {
		result, err := tc.reloader.CallTool(ctx, params)
		results <- callResult{result: result, err: err}
	}()
	return results
}

// sendChange delivers one watcher event to the loop.
func (tc *runContext) sendChange(t *testing.T) {
	t.Helper()

	select {
	case tc.events <- ChangeEvent{Path: "internal/server/server.go"}:
	case <-time.After(awaitDeadline):
		t.Fatal("timed out delivering a change event to the loop")
	}
}

// triggerChangeAndDebounce delivers one change event and fires its debounce
// timer, kicking off (or superseding into) a reload cycle.
func (tc *runContext) triggerChangeAndDebounce(t *testing.T) {
	t.Helper()

	tc.sendChange(t)
	tc.clock.awaitTimer(t, runDebounce).fire()
}

// sendTools delivers a runtime tool-change snapshot from child to the loop.
func (tc *runContext) sendTools(t *testing.T, child *testChild, tools []*mcp.Tool) {
	t.Helper()

	select {
	case child.tools <- tools:
	case <-time.After(awaitDeadline):
		t.Fatal("timed out delivering a runtime tool-change snapshot")
	}
}

// assertServedBy proves child is the live serving session: one armed call is
// routed to it and its result comes back. While the router is quiesced the
// call rides the swap buffer first, so this also awaits the drain.
func (tc *runContext) assertServedBy(t *testing.T, child *testChild, tool string) {
	t.Helper()

	want := textResult("served by the live child")
	child.mock.EXPECT().CallTool(mock.Anything, mock.Anything).Return(want, nil).Once()

	res := awaitResult(t, tc.startCall(t.Context(), callParams(tool)))

	require.NoError(t, res.err)
	assert.Same(t, want, res.result, "expected the call served by the expected child")
}

// testChild wraps a MockChildSession with test-owned lifecycle channels: the
// test crashes the child by closing done and emits runtime tool changes on
// tools.
type testChild struct {
	mock  *MockChildSession
	done  chan struct{}
	tools chan []*mcp.Tool
}

// newChild builds a child-session mock serving tools. The mock's expectations
// are asserted by start's cleanup after Run is joined, not by mockery's
// auto-cleanup, so shutdown-time Closes are counted.
func (tc *runContext) newChild(t *testing.T, tools []*mcp.Tool) *testChild {
	t.Helper()

	sessionMock := &MockChildSession{}
	sessionMock.Mock.Test(t)
	tc.children = append(tc.children, sessionMock)

	child := &testChild{
		mock:  sessionMock,
		done:  make(chan struct{}),
		tools: make(chan []*mcp.Tool, 1),
	}
	child.mock.EXPECT().Tools().Return(tools).Maybe()
	child.mock.EXPECT().Done().Return((<-chan struct{})(child.done)).Maybe()
	child.mock.EXPECT().ToolsChanged().Return((<-chan []*mcp.Tool)(child.tools)).Maybe()
	return child
}

// expectClose arms the child's exactly-once Close.
func (c *testChild) expectClose() {
	c.mock.EXPECT().Close().Return(nil).Once()
}

// crash simulates the child process dying unexpectedly.
func (c *testChild) crash() {
	close(c.done)
}

// toolSet builds a one-tool snapshot whose fingerprint is determined by name
// and version: equal arguments fingerprint equal, distinct versions differ.
func toolSet(name, version string) []*mcp.Tool {
	return []*mcp.Tool{{Name: name, Description: version}}
}

// logSignaler is a [slog.Handler] that signals (non-blocking) each record
// whose message matches. Tests use it to synchronize on loop transitions that
// have no port-level side effect to await — for example the noted-and-ignored
// mid-cycle crash, whose contract is router quiescence plus the log line.
type logSignaler struct {
	message string
	signals chan<- struct{}
}

func (s *logSignaler) Enabled(context.Context, slog.Level) bool { return true }

func (s *logSignaler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == s.message {
		select {
		case s.signals <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *logSignaler) WithAttrs([]slog.Attr) slog.Handler { return s }

func (s *logSignaler) WithGroup(string) slog.Handler { return s }
