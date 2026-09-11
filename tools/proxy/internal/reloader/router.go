package reloader

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// router owns the proxy's call path: it forwards downstream tool calls to the
// current child session and, while routing is quiesced for a swap or a crash
// restart, buffers them in a bounded queue with a per-call timeout.
//
// Its mutex is the core's only lock. Downstream handler goroutines enter
// through CallTool concurrently; Quiesce, Swap, SetFingerprints, Drain, and
// Close are called only from the orchestration loop goroutine but take the
// mutex because they touch the same call-path state.
//
// A router starts quiesced: until the first swap installs a child, arriving
// calls buffer instead of dispatching, so the call path never sees a nil
// child. The loop's first successful cycle runs Quiesce, Swap, Drain exactly
// like any later one.
type router struct {
	logger        *slog.Logger
	clock         Clock
	bufferLimit   int
	bufferTimeout time.Duration

	mu sync.Mutex
	// child is the session calls dispatch to. Nil only while quiesced
	// before the first swap.
	child ChildSession
	// generation counts swaps. Each dispatched call records the
	// generation it was issued under; a completion whose generation is
	// stale belongs to a superseded child.
	generation uint64
	// fingerprints maps tool name to the Fingerprint of the definition
	// the downstream client could currently see. Buffered calls record
	// their tool's entry at ingress; Drain gates release on it. Swap
	// deliberately does not replace the map — a call buffered between Swap
	// and Drain was issued against the definitions still advertised
	// downstream, so ingress must keep recording the old generation until
	// Drain installs the new one.
	fingerprints map[string]string
	quiesced     bool
	closed       bool
	// inflight counts calls dispatched to the current generation's child
	// and not yet completed. Swap resets it: superseded completions skip
	// the decrement.
	inflight int
	// drained is non-nil while a Quiesce waits for in-flight calls; it is
	// closed when inflight reaches zero or the wait is mooted by Swap or
	// Close.
	drained chan struct{}
	buffer  []*bufferedCall
}

// bufferedCall is one downstream call parked in the swap buffer. The released
// flag is guarded by router.mu and makes resolution exactly-once: whichever
// of Drain, Close, the per-call timeout, or caller cancellation claims the
// call first decides it.
type bufferedCall struct {
	name     string
	ingestFP string
	released bool
	release  chan releaseDecision // cap 1; sent exactly once
}

// releaseDecision is how the loop side resolves one buffered call. Exactly
// one field group applies: err fails the call (shutdown), stale answers it
// with the stale-reload result, and otherwise the caller goroutine dispatches
// to child under generation.
type releaseDecision struct {
	child      ChildSession
	generation uint64
	stale      bool
	err        error
}

// newRouter constructs the call router in its initial quiesced state.
func newRouter(logger *slog.Logger, clock Clock, bufferLimit int, bufferTimeout time.Duration) *router {
	return &router{
		logger:        logger,
		clock:         clock,
		bufferLimit:   bufferLimit,
		bufferTimeout: bufferTimeout,
		quiesced:      true,
	}
}

// CallTool routes one forwarded tool call per the CallToolFunc contract: a
// fresh CallToolParams carries only the name and the raw argument bytes, Meta
// (including any progress token) is dropped, and cancellation propagates via
// ctx. While quiesced the call buffers; otherwise it dispatches to the
// current child.
func (r *router) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	forwarded := &mcp.CallToolParams{Name: params.Name, Arguments: params.Arguments}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errShuttingDown
	}
	if !r.quiesced {
		child, generation := r.child, r.generation
		r.inflight++
		r.mu.Unlock()
		return r.dispatch(ctx, child, generation, forwarded)
	}
	if len(r.buffer) >= r.bufferLimit {
		r.mu.Unlock()
		r.logger.WarnContext(ctx, "rejecting tool call: swap buffer is full",
			"tool", forwarded.Name, "limit", r.bufferLimit)
		return bufferOverflowResult(forwarded.Name), nil
	}
	call := &bufferedCall{
		name:     forwarded.Name,
		ingestFP: r.fingerprints[forwarded.Name],
		release:  make(chan releaseDecision, 1),
	}
	r.buffer = append(r.buffer, call)
	r.mu.Unlock()

	return r.awaitRelease(ctx, call, forwarded)
}

// Quiesce stops routing new calls — they buffer instead — and reports when
// calls already in flight on the current child have drained. The returned
// channel is closed immediately when nothing is in flight; the orchestration
// loop bounds its wait with the quiesce grace timeout. Quiesce is idempotent:
// the crash path quiesces so restart-window calls buffer, and the following
// swap's own Quiesce simply observes the same state.
func (r *router) Quiesce() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.quiesced = true
	if r.inflight == 0 {
		done := make(chan struct{})
		close(done)
		return done
	}
	if r.drained == nil {
		r.drained = make(chan struct{})
	}
	return r.drained
}

// Swap atomically repoints the call path at candidate and returns the
// previous child (nil before the first swap) for the loop to close. Calls
// still in flight on the old child become superseded: their completions are
// answered with the friendly interrupted result. Routing stays quiesced — and
// ingress keeps recording the old generation's fingerprints — until Drain
// installs the new ones.
func (r *router) Swap(candidate ChildSession) ChildSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.child
	r.child = candidate
	r.generation++
	r.inflight = 0
	if r.drained != nil {
		close(r.drained)
		r.drained = nil
	}
	return old
}

// SetFingerprints replaces the recorded tool fingerprints without a swap. The
// loop calls it when a serving child changes its own tool set at runtime, so
// ingress recording reflects what the child now serves.
func (r *router) SetFingerprints(fingerprints map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fingerprints = fingerprints
}

// Drain installs the new generation's fingerprints, resumes direct routing,
// and releases every buffered call, gated on those fingerprints: a call is
// forwarded to the new child only when the new generation's definition for
// its tool is identical to the one recorded at ingress. A removed or changed
// tool — or one whose ingress definition was unknown or unfingerprintable —
// gets the stale-reload result instead: a non-idempotent call issued against
// old semantics must never silently execute on new code.
func (r *router) Drain(ctx context.Context, fingerprints map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fingerprints = fingerprints
	r.quiesced = false
	for _, call := range r.buffer {
		call.released = true
		if !r.matchesCurrentLocked(call) {
			r.logger.DebugContext(ctx, "answering buffered call with stale-reload: definition changed",
				"tool", call.name)
			call.release <- releaseDecision{stale: true}
			continue
		}
		r.inflight++
		call.release <- releaseDecision{child: r.child, generation: r.generation}
	}
	r.buffer = nil
}

// Close fails every buffered call with the shutdown error and rejects all
// future calls. The loop calls it once, on shutdown, after closing children.
func (r *router) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	for _, call := range r.buffer {
		call.released = true
		call.release <- releaseDecision{err: errShuttingDown}
	}
	r.buffer = nil
	if r.drained != nil {
		close(r.drained)
		r.drained = nil
	}
}

// awaitRelease parks one buffered call until the loop resolves it, its
// per-call timeout fires, or the caller cancels. The timeout and cancellation
// paths must claim the call first: if a release decision won the race it is
// honored, because Drain may already have charged the call against the
// in-flight counter.
func (r *router) awaitRelease(
	ctx context.Context,
	call *bufferedCall,
	params *mcp.CallToolParams,
) (*mcp.CallToolResult, error) {
	timeout := r.clock.After(r.bufferTimeout)
	select {
	case decision := <-call.release:
		return r.applyDecision(ctx, decision, params)
	case <-timeout:
		if decision, released := r.claimCancellation(call); released {
			return r.applyDecision(ctx, decision, params)
		}
		r.logger.WarnContext(ctx, "buffered tool call timed out waiting for reload", "tool", params.Name)
		return bufferTimeoutResult(params.Name), nil
	case <-ctx.Done():
		if decision, released := r.claimCancellation(call); released {
			return r.applyDecision(ctx, decision, params)
		}
		return nil, ctx.Err()
	}
}

// applyDecision turns one release decision into the buffered call's outcome
// on the caller's goroutine.
func (r *router) applyDecision(
	ctx context.Context,
	decision releaseDecision,
	params *mcp.CallToolParams,
) (*mcp.CallToolResult, error) {
	switch {
	case decision.err != nil:
		return nil, decision.err
	case decision.stale:
		return StaleReloadResult(params.Name), nil
	default:
		return r.dispatch(ctx, decision.child, decision.generation, params)
	}
}

// claimCancellation atomically claims a buffered call for its timeout or
// cancellation path. When the loop already released the call, the pending
// decision is returned instead and must be honored by the caller.
func (r *router) claimCancellation(call *bufferedCall) (releaseDecision, bool) {
	r.mu.Lock()
	if call.released {
		r.mu.Unlock()
		return <-call.release, true
	}
	call.released = true
	if i := slices.Index(r.buffer, call); i >= 0 {
		r.buffer = slices.Delete(r.buffer, i, i+1)
	}
	r.mu.Unlock()
	return releaseDecision{}, false
}

// dispatch forwards one call to child and settles the call-path bookkeeping:
// a completion from the current generation decrements the in-flight count
// (closing a pending quiesce drain at zero), while any completion from a
// superseded child — error or late success — is answered with the friendly
// interrupted result. Calls stranded past the quiesce grace get an error
// result: the call may have executed on the old code, and the caller is told
// to verify rather than handed output from a definition the reload has since
// replaced.
func (r *router) dispatch(
	ctx context.Context,
	child ChildSession,
	generation uint64,
	params *mcp.CallToolParams,
) (*mcp.CallToolResult, error) {
	result, err := child.CallTool(ctx, params)

	r.mu.Lock()
	superseded := generation != r.generation
	if !superseded {
		r.inflight--
		if r.inflight == 0 && r.drained != nil {
			close(r.drained)
			r.drained = nil
		}
	}
	r.mu.Unlock()

	if superseded {
		r.logger.WarnContext(ctx, "tool call completed on a superseded child; answering with interrupted result",
			"tool", params.Name, "error", err)
		return supersededResult(params.Name), nil
	}
	return result, err
}

// matchesCurrentLocked reports whether a buffered call's ingress fingerprint
// matches the current generation's definition for the same tool. An unknown
// ingress fingerprint never matches, and neither does an error marker from
// fingerprintTools — both gate stale rather than silently matching.
func (r *router) matchesCurrentLocked(call *bufferedCall) bool {
	if call.ingestFP == "" || isErrorFingerprint(call.ingestFP) {
		return false
	}
	return r.fingerprints[call.name] == call.ingestFP
}
