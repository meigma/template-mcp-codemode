package reloader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errSwapAborted signals that performSwap observed shutdown while waiting for
// in-flight calls to drain. The candidate has already been closed; the run
// loop's shutdown path owns the serving child and the router.
var errSwapAborted = errors.New("swap aborted by shutdown")

// cycleResult carries one cycle goroutine's outcome back to the run loop.
// child is nil on failure; artifact is the path that was started (or would
// have been, had Start succeeded).
type cycleResult struct {
	child    ChildSession
	artifact string
	err      error
}

// Run executes the orchestration loop until ctx is cancelled. It returns nil
// on clean shutdown and an error when the frontend is unset or the watcher
// fails. Run is single-use.
//
// On entry Run kicks off the first build cycle immediately — the downstream
// adapter is already serving the empty tool set independently, so a broken
// first build never blocks the client's session. From then on it serializes
// the debounce, build, health-gate, swap, reconcile, drain lifecycle,
// supervises child crashes with exponential backoff, and reconciles child
// runtime tool changes without a restart. On ctx cancellation it cancels any
// in-flight cycle, closes every child it owns (serving and mid-swap
// candidate), and fails buffered calls — no orphans.
func (r *Reloader) Run(ctx context.Context) error {
	if r.frontend == nil {
		return errors.New("frontend is required: call SetFrontend before Run")
	}
	events, err := r.watcher.Watch(ctx)
	if err != nil {
		return fmt.Errorf("watch source tree: %w", err)
	}

	loop := &runLoop{r: r, events: events}
	r.logger.InfoContext(ctx, "cold start: building the first child", "state", "BUILDING")
	loop.startCycle(ctx, "")
	return loop.run(ctx)
}

// runCycle is the only goroutine the core spawns besides the run loop, and at
// most one is alive at a time. It runs the slow, cancellable cycle parts —
// Build (skipped when artifact is non-empty: a crash restart reuses the last
// good artifact) and Start — and always sends exactly one result on the cap-1
// results channel. It never blocks and never closes children; the run loop is
// the sole owner of every Close.
func (r *Reloader) runCycle(ctx context.Context, artifact string, results chan<- cycleResult) {
	if artifact == "" {
		build, err := r.builder.Build(ctx)
		if err != nil {
			results <- cycleResult{err: fmt.Errorf("build: %w", err)}
			return
		}
		if build.Output != "" {
			r.logger.DebugContext(ctx, "build output", "output", build.Output)
		}
		artifact = build.Artifact
	}
	child, err := r.upstream.Start(ctx, artifact)
	if err != nil {
		results <- cycleResult{artifact: artifact, err: fmt.Errorf("start child %q: %w", artifact, err)}
		return
	}
	results <- cycleResult{child: child, artifact: artifact}
}

// performSwap runs the quiesce, swap, reconcile, drain sequence inline in the
// run loop — swap is the critical section the loop serializes anyway, and the
// wait is bounded by the quiesce grace. It returns the new tool set's
// fingerprints and whether the frontend now advertises that set.
//
// The drained channel is checked non-blocking first: in the common idle case
// Quiesce returns a pre-closed channel and no grace timer is ever created.
// Reconcile is skipped entirely when fingerprintsEqual reports the new
// fingerprint set unchanged — an identical tool set produces zero Frontend
// calls; error-marker fingerprints never count as unchanged — unless
// frontendStale reports that the last Reconcile failed, in which case the
// advertised set cannot be trusted to match and the skip is suppressed. A
// Reconcile error is logged loudly and the new child serves anyway: its
// definitions were already validated by the health gate, and dev-loop
// availability beats killing a healthy child. The router's fingerprints are
// installed by Drain, not Swap,
// so calls buffered mid-swap record the definitions the client could still
// see at ingress. On ctx cancellation during the quiesce wait the candidate
// is closed and errSwapAborted returned.
func (r *Reloader) performSwap(
	ctx context.Context,
	candidate ChildSession,
	currentFPs map[string]string,
	frontendStale bool,
) (map[string]string, bool, error) {
	r.logger.InfoContext(ctx, "swapping in the new child", "state", "SWAPPING")
	drained := r.router.Quiesce()
	select {
	case <-drained:
	default:
		select {
		case <-drained:
		case <-r.clock.After(r.quiesceGrace):
			r.logger.WarnContext(ctx,
				"quiesce grace elapsed: swapping anyway; in-flight calls will be superseded",
				"grace", r.quiesceGrace)
		case <-ctx.Done():
			r.closeChild(ctx, candidate, "candidate")
			return nil, false, errSwapAborted
		}
	}

	tools := candidate.Tools()
	fps := fingerprintTools(ctx, r.logger, tools)
	if old := r.router.Swap(candidate); old != nil {
		r.closeChild(ctx, old, "previous")
	}
	reconciled := true
	if !frontendStale && fingerprintsEqual(fps, currentFPs) {
		r.logger.DebugContext(ctx, "tool set unchanged: skipping reconcile")
	} else if err := r.frontend.Reconcile(tools, r.CallTool); err != nil {
		reconciled = false
		r.logger.ErrorContext(ctx,
			"reconcile after swap failed; serving the new child anyway and retrying on the next reconcile",
			"error", err)
	}
	r.router.Drain(ctx, fps)
	return fps, reconciled, nil
}

// nextBackoff advances the retry delay: from the floor when current is zero,
// otherwise doubling up to the ceiling.
func (r *Reloader) nextBackoff(current time.Duration) time.Duration {
	if current == 0 {
		return r.backoffFloor
	}
	return min(current*backoffFactor, r.backoffCeiling)
}

// closeChild closes one child session, logging instead of propagating the
// error: a child that fails to close cleanly must never stall the loop.
func (r *Reloader) closeChild(ctx context.Context, child ChildSession, role string) {
	if err := child.Close(); err != nil {
		r.logger.WarnContext(ctx, "closing child session failed", "role", role, "error", err)
	}
}

// runLoop is the orchestration state, owned exclusively by the single Run
// goroutine. There is no state-machine enum: the lifecycle states
// (SERVING -> BUILDING -> STARTING -> SWAPPING -> SERVING) appear only as slog
// attributes on transition logs, and a nil channel disables its select case
// (currentDone, currentTools, debounceCh, retryCh).
type runLoop struct {
	r      *Reloader
	events <-chan ChangeEvent

	// current is the serving child; nil until the first swap.
	current      ChildSession
	currentDone  <-chan struct{}
	currentTools <-chan []*mcp.Tool
	// currentDead records that current's Done fired: mid-cycle it is noted
	// and absorbed (the cycle replaces the child); it also makes a failed
	// cycle schedule a retry instead of "keeping" a dead child.
	currentDead bool
	currentFPs  map[string]string
	// frontendStale records that the last Reconcile failed: the advertised
	// set may not match currentFPs, so the next swap or runtime tool change
	// must reconcile even when the fingerprints are identical.
	frontendStale bool

	// cycleCancel is non-nil exactly while a cycle goroutine is in flight.
	cycleCancel  context.CancelFunc
	cycleResults chan cycleResult
	// cycleFresh records whether the in-flight cycle runs a full build of
	// fresh source (as opposed to a build-free restart of an old artifact);
	// onCycleResult uses it to decide whether success clears the backoff.
	cycleFresh bool
	// rerun marks a cancelled-and-superseded cycle: its result triggers
	// exactly one fresh cycle.
	rerun bool

	debounceCh <-chan time.Time
	retryCh    <-chan time.Time
	// retryArtifact is what a backoff retry starts: empty means a full
	// build, otherwise the last good artifact is restarted build-free.
	retryArtifact string
	// backoff is the current exponential restart delay. It advances on every
	// cycle failure with no healthy child and on every build-free restart's
	// success, and clears only when a fresh build cycle succeeds — so a
	// crash-looping child is paced even when each restart health-gates.
	backoff      time.Duration
	lastArtifact string
}

// run is the select loop. Every Close of every child happens here (or in
// helpers it calls synchronously); the cycle goroutine only builds and starts.
func (l *runLoop) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			l.shutdown(ctx)
			return nil

		case ev, ok := <-l.events:
			if !ok {
				l.shutdown(ctx)
				if ctx.Err() != nil {
					return nil //nolint:nilerr // ctx cancellation is a clean shutdown, not an error.
				}
				return errors.New("watcher channel closed unexpectedly")
			}
			l.onChange(ctx, ev)

		case <-l.debounceCh:
			l.onDebounce(ctx)

		case res := <-l.cycleResults:
			if err := l.onCycleResult(ctx, res); err != nil {
				// Only errSwapAborted reaches here: shutdown interrupted the
				// swap, which is a clean exit, not a failure.
				l.shutdown(ctx)
				return nil //nolint:nilerr // Shutdown mid-swap is the normal signal-driven exit.
			}

		case <-l.currentDone:
			l.onChildDied(ctx)

		case <-l.retryCh:
			l.retryCh = nil
			if l.cycleCancel != nil {
				// Never overwrite an in-flight cycle: it owns recovery, and
				// its failure path re-arms the retry. onChange and onDebounce
				// clear a pending retry, so this is a last-resort guard.
				continue
			}
			l.r.logger.InfoContext(ctx, "retrying reload cycle",
				"state", "BUILDING", "artifact", l.retryArtifact, "backoff", l.backoff)
			l.startCycle(ctx, l.retryArtifact)

		case tools, ok := <-l.currentTools:
			l.onToolsChanged(ctx, tools, ok)
		}
	}
}

// onChange debounces one raw watcher event. A pending backoff retry is
// dropped: fresh source supersedes a restart of the old artifact, and the
// debounced cycle does a full build. Debounce reset is by abandonment — each
// event arms a new timer and the previous channel is forgotten.
func (l *runLoop) onChange(ctx context.Context, ev ChangeEvent) {
	l.r.logger.DebugContext(ctx, "source change detected: debouncing", "path", ev.Path)
	l.debounceCh = l.r.clock.After(l.r.debounce)
	l.retryCh = nil
}

// onDebounce starts a reload cycle, or cancels-and-supersedes the one in
// flight so exactly one final cycle runs against the newest source. A pending
// backoff retry is dropped for the same reason onChange drops it: the fresh
// cycle supersedes any restart of the old artifact, and a retry firing into
// an in-flight cycle would race two children for one downstream session.
func (l *runLoop) onDebounce(ctx context.Context) {
	l.debounceCh = nil
	l.retryCh = nil
	if l.cycleCancel != nil {
		l.r.logger.InfoContext(ctx, "change during reload cycle: cancelling and superseding")
		l.cycleCancel()
		l.rerun = true
		return
	}
	l.r.logger.InfoContext(ctx, "change debounced: starting reload cycle", "state", "BUILDING")
	l.startCycle(ctx, "")
}

// onCycleResult settles one finished cycle: superseded results are discarded
// (closing the stale candidate) and rerun once, failures keep the old child
// or schedule a backoff retry when no healthy child serves, and successes
// swap. A non-nil return means shutdown interrupted the swap.
func (l *runLoop) onCycleResult(ctx context.Context, res cycleResult) error {
	// The cycle goroutine already sent its result, so cancelling is
	// side-effect-free — but skipping it would leak the cancel context into
	// the parent ctx's children for the life of Run, one node per cycle.
	l.cycleCancel()
	l.cycleCancel = nil
	l.cycleResults = nil

	if l.rerun {
		l.rerun = false
		if res.child != nil {
			// Even a successful candidate was built from stale source.
			l.r.closeChild(ctx, res.child, "superseded candidate")
		}
		l.r.logger.InfoContext(ctx, "running superseding cycle", "state", "BUILDING")
		l.startCycle(ctx, "")
		return nil
	}
	if res.err != nil {
		l.onCycleFailure(ctx, res.err)
		return nil //nolint:nilerr // A failed cycle keeps serving (or retries); it never stops the loop.
	}

	fps, reconciled, err := l.r.performSwap(ctx, res.child, l.currentFPs, l.frontendStale)
	if err != nil {
		return err
	}
	l.current = res.child
	l.currentFPs = fps
	l.frontendStale = !reconciled
	l.currentDone = res.child.Done()
	l.currentTools = res.child.ToolsChanged()
	l.currentDead = false
	l.lastArtifact = res.artifact
	// Only a fresh build clears the backoff. A successful build-free restart
	// advances it instead: a crash-looping child "succeeds" every restart, and
	// resetting here would respawn it in a tight loop forever (the §5 failure
	// row "auto-restart with backoff" exists to bound exactly that).
	if l.cycleFresh {
		l.backoff = 0
	} else {
		l.backoff = l.r.nextBackoff(l.backoff)
	}
	l.r.logger.InfoContext(ctx, "serving the new child", "state", "SERVING", "artifact", res.artifact)
	return nil
}

// onCycleFailure keeps the old child serving when it is healthy — the next
// save retriggers — and otherwise schedules a backoff retry so a failed first
// build or a dead child is never stranded. With no healthy child the router
// is quiesced first, so retry-window calls buffer instead of dispatching to a
// dead transport — the same treatment the crash-in-SERVING window gets. The
// retry restarts the last good artifact build-free; before any child ever
// served, lastArtifact is empty and the retry runs a full build.
func (l *runLoop) onCycleFailure(ctx context.Context, cycleErr error) {
	l.r.logger.ErrorContext(ctx, "reload cycle failed", "error", cycleErr)
	if l.current != nil && !l.currentDead {
		l.r.logger.InfoContext(ctx, "keeping the current child", "state", "SERVING")
		return
	}
	l.r.router.Quiesce()
	l.backoff = l.r.nextBackoff(l.backoff)
	l.retryArtifact = l.lastArtifact
	l.retryCh = l.r.clock.After(l.backoff)
	l.r.logger.WarnContext(ctx, "no healthy child: retry scheduled", "backoff", l.backoff)
}

// onChildDied handles the serving child's Done firing. The router is quiesced
// immediately in every case, so crash-window calls buffer instead of
// dispatching to the dead child's transport. Mid-cycle the crash is then
// noted and otherwise ignored — the in-flight cycle already replaces the
// child, and its swap's Drain releases the buffered calls. In SERVING it
// restarts the last artifact build-free: immediately when the backoff is
// clear (the child came from a fresh build), otherwise after the current
// backoff delay. Every build-free restart advances the backoff
// (onCycleResult) whether or not it health-gates, so consecutive crashes are
// paced exponentially until a fresh build resets the delay.
func (l *runLoop) onChildDied(ctx context.Context) {
	l.currentDone = nil
	l.currentDead = true
	l.r.router.Quiesce()
	if l.cycleCancel != nil {
		l.r.logger.WarnContext(ctx, "child died during an in-flight reload cycle; the cycle replaces it")
		return
	}
	if l.backoff > 0 {
		l.retryArtifact = l.lastArtifact
		l.retryCh = l.r.clock.After(l.backoff)
		l.r.logger.ErrorContext(ctx, "child died while serving: restart scheduled with backoff",
			"backoff", l.backoff, "artifact", l.lastArtifact)
		return
	}
	l.r.logger.ErrorContext(ctx, "child died while serving: restarting immediately",
		"state", "STARTING", "artifact", l.lastArtifact)
	l.startCycle(ctx, l.lastArtifact)
}

// onToolsChanged reconciles a child runtime tool change without a restart. An
// identical snapshot is skipped outright (error-marker fingerprints never
// count as identical) — unless the last Reconcile failed, in which case the
// advertised set may not match and the skip is suppressed. Otherwise the
// router's fingerprints are updated first, so ingress recording reflects what
// the child now serves, then the frontend reconciles. A Reconcile error is
// logged loudly, the child keeps serving, and the failure is recorded so the
// next opportunity retries.
func (l *runLoop) onToolsChanged(ctx context.Context, tools []*mcp.Tool, ok bool) {
	if !ok {
		l.currentTools = nil
		return
	}
	fps := fingerprintTools(ctx, l.r.logger, tools)
	if !l.frontendStale && fingerprintsEqual(fps, l.currentFPs) {
		l.r.logger.DebugContext(ctx, "child runtime tool change is identical: skipping reconcile")
		return
	}
	l.r.router.SetFingerprints(fps)
	l.frontendStale = false
	if err := l.r.frontend.Reconcile(tools, l.r.CallTool); err != nil {
		l.frontendStale = true
		l.r.logger.ErrorContext(ctx,
			"reconcile after child runtime tool change failed; still serving and retrying on the next reconcile",
			"error", err)
	} else {
		l.r.logger.InfoContext(ctx, "reconciled child runtime tool change", "tools", len(tools))
	}
	l.currentFPs = fps
}

// startCycle launches the cycle goroutine. An empty artifact means a full
// build; otherwise the artifact is restarted build-free.
func (l *runLoop) startCycle(ctx context.Context, artifact string) {
	cycleCtx, cancel := context.WithCancel(ctx)
	l.cycleCancel = cancel
	l.cycleResults = make(chan cycleResult, 1)
	l.cycleFresh = artifact == ""
	go l.r.runCycle(cycleCtx, artifact, l.cycleResults)
}

// shutdown cancels any in-flight cycle and closes every child the loop owns —
// the candidate a cancelled cycle may still deliver, then the serving child —
// before failing buffered calls via the router. SWAPPING briefly owns two
// children; both are closed.
func (l *runLoop) shutdown(ctx context.Context) {
	if l.cycleCancel != nil {
		l.cycleCancel()
		if res := <-l.cycleResults; res.child != nil {
			l.r.closeChild(ctx, res.child, "candidate")
		}
		l.cycleCancel = nil
	}
	if l.current != nil {
		l.r.closeChild(ctx, l.current, "serving")
	}
	l.r.router.Close()
	l.r.logger.InfoContext(ctx, "dev proxy core shut down cleanly")
}
