package reloader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Default values for the Options timing and sizing knobs. They are first-pass
// dev-loop numbers; tuning is deliberately deferred, and the CLI exposes flags
// to override them.
const (
	// defaultDebounce is how long source-change events are coalesced before
	// a reload cycle starts.
	defaultDebounce = 300 * time.Millisecond

	// defaultQuiesceGrace bounds a swap's wait for calls in flight on the
	// old child to drain.
	defaultQuiesceGrace = 5 * time.Second

	// defaultBufferLimit bounds the swap buffer: calls beyond it are
	// answered with an error result instead of queueing without bound.
	defaultBufferLimit = 32

	// defaultBufferTimeout bounds how long one buffered call waits for a
	// stalled reload before it is answered with an error result.
	defaultBufferTimeout = 10 * time.Second

	// defaultBackoffFloor is the first retry delay when no healthy child
	// from fresh source is serving (a failed first build, a crash restart
	// that fails, or a child that keeps crashing after each restart).
	defaultBackoffFloor = 250 * time.Millisecond

	// defaultBackoffCeiling caps the exponential growth of the retry delay.
	defaultBackoffCeiling = 5 * time.Second

	// backoffFactor doubles the retry delay after each consecutive failure.
	backoffFactor = 2
)

// Options configures a Reloader for [New].
type Options struct {
	// Watcher streams raw source-change events for the core to debounce.
	// Required.
	Watcher Watcher

	// Builder produces the unique child artifact for each reload cycle.
	// Required.
	Builder Builder

	// Upstream spawns and health-gates child MCP sessions from built
	// artifacts. Required.
	Upstream Upstream

	// Logger receives orchestration diagnostics. Nil selects a no-op
	// logger.
	//
	// WARNING: a logger must never write to [os.Stdout]. Stdout is the
	// JSON-RPC protocol channel on both of the proxy's hops; stderr is
	// always safe.
	Logger *slog.Logger

	// Clock supplies debounce and backoff timers. Nil selects real time.
	Clock Clock

	// Debounce is how long the core waits after a source-change event
	// before starting a reload cycle; every fresh event restarts the wait,
	// coalescing save bursts into one rebuild. Zero selects the default of
	// 300ms; negative values are rejected by [New].
	Debounce time.Duration

	// QuiesceGrace bounds how long a swap waits for calls in flight on the
	// old child to finish before proceeding anyway; calls still running
	// past it are superseded and answered with an error result. Zero
	// selects the default of 5s; negative values are rejected by [New].
	QuiesceGrace time.Duration

	// BufferLimit caps how many tool calls may wait in the swap buffer
	// while routing is quiesced (mid-swap or during a crash-restart
	// window). Excess calls receive an error result immediately, so the
	// downstream session never blocks on an unbounded queue. Zero selects
	// the default of 32; negative values are rejected by [New].
	BufferLimit int

	// BufferTimeout bounds how long one buffered tool call waits for a
	// swap to finish before receiving an error result. Zero selects the
	// default of 10s; negative values are rejected by [New].
	BufferTimeout time.Duration

	// BackoffFloor is the first delay of the exponential backoff that paces
	// recovery restarts: a failed first build, a crashed child whose
	// restart fails, or a child that crashes again after a restart.
	// Consecutive failures and crash restarts double the delay. Zero
	// selects the default of 250ms; negative values are rejected by [New].
	BackoffFloor time.Duration

	// BackoffCeiling caps the exponential retry delay. Only a fresh build
	// cycle's success clears the delay; a successful build-free crash
	// restart advances it instead, so a crash-looping child is paced even
	// when every restart health-gates. Zero selects the default of 5s;
	// negative values are rejected by [New].
	BackoffCeiling time.Duration
}

// Reloader is the dev proxy's core orchestrator.
//
// It owns the watch, debounce, build, health-gate, swap cycle that keeps a
// persistent downstream MCP session pointed at a disposable child server.
// Construct it with [New], wire the client-facing side with
// [Reloader.SetFrontend], then drive it with [Reloader.Run].
type Reloader struct {
	watcher  Watcher
	builder  Builder
	upstream Upstream
	logger   *slog.Logger
	clock    Clock

	debounce       time.Duration
	quiesceGrace   time.Duration
	backoffFloor   time.Duration
	backoffCeiling time.Duration

	frontend Frontend
	router   *router
}

// New constructs a Reloader from the provided Options.
//
// The Watcher, Builder, and Upstream ports are required; New returns an
// error when any of them is nil. A nil Logger selects a no-op logger and a
// nil Clock selects real time. The returned Reloader still needs its
// client-facing side wired via [Reloader.SetFrontend] before it can run.
func New(options Options) (*Reloader, error) {
	if options.Watcher == nil {
		return nil, errors.New("watcher is required")
	}
	if options.Builder == nil {
		return nil, errors.New("builder is required")
	}
	if options.Upstream == nil {
		return nil, errors.New("upstream is required")
	}
	if options.BufferLimit < 0 {
		return nil, errors.New("buffer limit must not be negative")
	}
	for name, value := range map[string]time.Duration{
		"debounce":        options.Debounce,
		"quiesce grace":   options.QuiesceGrace,
		"buffer timeout":  options.BufferTimeout,
		"backoff floor":   options.BackoffFloor,
		"backoff ceiling": options.BackoffCeiling,
	} {
		if value < 0 {
			return nil, fmt.Errorf("%s must not be negative", name)
		}
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	clock := options.Clock
	if clock == nil {
		clock = systemClock{}
	}
	bufferLimit := options.BufferLimit
	if bufferLimit == 0 {
		bufferLimit = defaultBufferLimit
	}

	return &Reloader{
		watcher:        options.Watcher,
		builder:        options.Builder,
		upstream:       options.Upstream,
		logger:         logger,
		clock:          clock,
		debounce:       defaultDuration(options.Debounce, defaultDebounce),
		quiesceGrace:   defaultDuration(options.QuiesceGrace, defaultQuiesceGrace),
		backoffFloor:   defaultDuration(options.BackoffFloor, defaultBackoffFloor),
		backoffCeiling: defaultDuration(options.BackoffCeiling, defaultBackoffCeiling),
		router: newRouter(
			logger,
			clock,
			bufferLimit,
			defaultDuration(options.BufferTimeout, defaultBufferTimeout),
		),
	}, nil
}

// CallTool is the core's [CallToolFunc]: it routes one forwarded tool call to
// the current child session, or parks it in the swap buffer while routing is
// quiesced for a swap or crash restart.
//
// Per the CallToolFunc contract, the forwarded call carries only the tool
// name and the raw argument bytes — Meta, including any progress token, is
// dropped — and cancellation propagates via ctx. The downstream adapter is
// constructed with this method value as every passthrough tool's handler.
func (r *Reloader) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return r.router.CallTool(ctx, params)
}

// SetFrontend wires the client-facing side the core reconciles tool sets
// against. It must be called before the orchestration loop runs.
//
// SetFrontend exists to resolve the construction cycle between the core and
// the downstream adapter: build the core first, construct the downstream
// adapter with the core's router (a [CallToolFunc]), then hand the adapter
// back here. The Frontend never needs the core type, only the CallToolFunc.
func (r *Reloader) SetFrontend(frontend Frontend) {
	r.frontend = frontend
}

// defaultDuration substitutes def for an unset (zero) duration knob.
func defaultDuration(value, def time.Duration) time.Duration {
	if value == 0 {
		return def
	}
	return value
}
