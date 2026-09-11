package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// artifactToken is the placeholder in the child argv template replaced with
// the cycle's artifact path.
const artifactToken = "{{artifact}}"

// clientName identifies the proxy on the upstream hop: each child gets a
// fresh, normal initialize under the proxy's own identity rather than a
// replay of the downstream client's init params.
const clientName = "mcp-devproxy"

// clientVersion is the version the proxy reports to its children.
const clientVersion = "dev"

// defaultTerminateDuration replaces the SDK's 5s CommandTransport escalation
// window when Options.TerminateDuration is zero: a hung old child in a dev
// loop should be SIGTERMed within a second, not five.
const defaultTerminateDuration = time.Second

// defaultHealthTimeout bounds each network step of Start (connect, logging
// replay, tool listing) and the re-list after a child tools/list_changed
// when Options.HealthTimeout is zero.
const defaultHealthTimeout = 5 * time.Second

// Method strings for the server-to-client features the v1 proxy does not
// forward, mirroring the SDK's unexported constants.
const (
	methodListRoots     = "roots/list"
	methodCreateMessage = "sampling/createMessage"
	methodElicit        = "elicitation/create"
)

// TransportFactory produces the transport that connects to one child for the
// given artifact path. The default factory builds an mcp.CommandTransport
// running the configured argv; integration tests inject the client side of
// mcp.NewInMemoryTransports instead.
type TransportFactory func(artifact string) (mcp.Transport, error)

// Options configures an Upstream for New.
type Options struct {
	// Argv is the child command template; every occurrence of {{artifact}}
	// is replaced, per element, with the cycle's artifact path. Required
	// unless Transport is set.
	Argv []string

	// TerminateDuration is how long the default transport's Close waits at
	// each shutdown escalation step (stdin close, SIGTERM, SIGKILL). Zero
	// selects a dev-loop-short 1s; negative is rejected by New.
	TerminateDuration time.Duration

	// HealthTimeout bounds each network step of Start individually —
	// connect (which covers the child's initialize), the logging-level
	// replay, and the health gate's tool listing — and the re-list after a
	// child emits tools/list_changed. Each step gets its own
	// HealthTimeout-derived deadline rather than sharing one Start-wide
	// budget, so a child that hangs at any step cannot stall Start beyond
	// that step's bound. Zero selects 5s.
	HealthTimeout time.Duration

	// LogHandler receives the child's notifications/message params for
	// forwarding downstream. Nil drops them; child stderr remains the
	// primary debug channel either way.
	LogHandler func(context.Context, *mcp.LoggingMessageParams)

	// LevelProvider returns the downstream client's last logging/setLevel,
	// replayed to each new child right after connect. Nil, or an empty
	// level, skips the replay.
	LevelProvider func() mcp.LoggingLevel

	// Stderr is where each child's stderr is wired — the CommandTransport
	// does not do this itself, and the developer must see their server's
	// logs. Nil selects the proxy's os.Stderr.
	Stderr io.Writer

	// Transport overrides the default CommandTransport factory; tests use
	// it to inject in-memory children. Nil selects the default factory.
	Transport TransportFactory

	// Logger receives operational logs. Nil selects a no-op logger.
	Logger *slog.Logger
}

// Upstream is the mcp.Client-backed implementation of the reloader.Upstream
// port. Each Start launches, connects, and health-gates one disposable child
// session.
type Upstream struct {
	argv          []string
	terminate     time.Duration
	healthTimeout time.Duration
	logHandler    func(context.Context, *mcp.LoggingMessageParams)
	levelProvider func() mcp.LoggingLevel
	stderr        io.Writer
	factory       TransportFactory
	logger        *slog.Logger
}

// New constructs an Upstream from options.
//
// New fails when neither Argv nor Transport is set — without one of them
// there is no way to reach a child — and when TerminateDuration is negative,
// which would be handed untouched to the transport's shutdown escalation.
// Unset options select dev-loop defaults: a 1s TerminateDuration, a 5s
// HealthTimeout, the proxy's own [os.Stderr] for child stderr, the
// CommandTransport factory, and a no-op logger.
func New(options Options) (*Upstream, error) {
	if options.Transport == nil && len(options.Argv) == 0 {
		return nil, errors.New("a child argv template is required when no transport factory is set")
	}
	if options.TerminateDuration < 0 {
		return nil, errors.New("terminate duration must not be negative")
	}

	upstream := &Upstream{
		argv:          options.Argv,
		terminate:     options.TerminateDuration,
		healthTimeout: options.HealthTimeout,
		logHandler:    options.LogHandler,
		levelProvider: options.LevelProvider,
		stderr:        options.Stderr,
		factory:       options.Transport,
		logger:        options.Logger,
	}
	if upstream.terminate == 0 {
		upstream.terminate = defaultTerminateDuration
	}
	if upstream.healthTimeout == 0 {
		upstream.healthTimeout = defaultHealthTimeout
	}
	if upstream.stderr == nil {
		upstream.stderr = os.Stderr
	}
	if upstream.factory == nil {
		upstream.factory = upstream.commandTransport
	}
	if upstream.logger == nil {
		upstream.logger = slog.New(slog.DiscardHandler)
	}
	return upstream, nil
}

// Start launches the artifact's child, connects to it under the proxy's
// identity, replays the downstream client's last logging level, and
// health-gates the result: the child's tools are listed and every definition
// validated. Each network step runs under its own HealthTimeout-derived
// deadline (see Options.HealthTimeout), so a child that hangs during
// initialize cannot stall Start. Any failure tears down the half-started
// child and returns an error — the core keeps the old child serving.
func (u *Upstream) Start(ctx context.Context, artifact string) (reloader.ChildSession, error) {
	transport, err := u.factory(artifact)
	if err != nil {
		return nil, fmt.Errorf("create child transport: %w", err)
	}

	child := &childSession{
		toolsCh:       make(chan []*mcp.Tool, 1),
		done:          make(chan struct{}),
		ready:         make(chan struct{}),
		logHandler:    u.logHandler,
		relistTimeout: u.healthTimeout,
		logger:        u.logger,
	}

	// A fresh client per child: upstream sessions are disposable and the
	// handlers must close over this child. The empty ClientCapabilities
	// stops advertising roots; that alone is insufficient — the SDK's
	// built-in handler answers roots/list with an empty list regardless of
	// what was advertised — so the middleware below rejects it loudly.
	client := mcp.NewClient(
		&mcp.Implementation{Name: clientName, Version: clientVersion},
		&mcp.ClientOptions{
			Capabilities:           &mcp.ClientCapabilities{},
			ToolListChangedHandler: child.onToolListChanged,
			LoggingMessageHandler:  child.onLoggingMessage,
		},
	)
	client.AddReceivingMiddleware(u.fidelityGapMiddleware())

	// A deadline-cancelled Connect leaves no orphan: the SDK closes the
	// session when initialize fails, and CommandTransport's Close runs the
	// stdin-close → SIGTERM → SIGKILL ladder (go-sdk v1.6.1: client.go
	// Connect, cmd.go pipeRWC.Close). The cancel right after Connect is
	// safe too — jsonrpc2 detaches the session's read loop from this ctx.
	connectCtx, cancelConnect := context.WithTimeout(ctx, u.healthTimeout)
	session, err := client.Connect(connectCtx, transport, nil)
	cancelConnect()
	if err != nil {
		return nil, fmt.Errorf("connect to child: %w", err)
	}
	// Closing ready is the happens-before barrier that lets notification
	// handlers use the session; a tools/list_changed arriving earlier is
	// dropped, correctly, because the health gate's own list runs later and
	// captures the final set.
	child.session = session
	close(child.ready)

	go child.watchDone()

	u.replayLoggingLevel(ctx, session)

	tools, err := u.healthGate(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	child.tools = tools

	u.warnUnforwardedCapabilities(ctx, session)

	return child, nil
}

// replayLoggingLevel re-sends the downstream client's last logging/setLevel
// to the new child so child log messages flow at the level the client
// already chose. The replay is optional and bounded by the health timeout:
// a child without logging support must not fail the gate, and a child that
// never answers must not stall Start, so failures are logged and ignored.
func (u *Upstream) replayLoggingLevel(ctx context.Context, session *mcp.ClientSession) {
	if u.levelProvider == nil {
		return
	}
	level := u.levelProvider()
	if level == "" {
		return
	}
	replayCtx, cancel := context.WithTimeout(ctx, u.healthTimeout)
	defer cancel()
	if err := session.SetLoggingLevel(replayCtx, &mcp.SetLoggingLevelParams{Level: level}); err != nil {
		u.logger.WarnContext(ctx, "replaying the logging level to the new child failed",
			"level", level, "error", err)
	}
}

// healthGate lists the child's tools under the health timeout and validates
// every definition: a child whose advertised tools cannot safely be
// registered downstream never serves.
func (u *Upstream) healthGate(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	gateCtx, cancel := context.WithTimeout(ctx, u.healthTimeout)
	defer cancel()

	tools, err := listTools(gateCtx, session)
	if err != nil {
		return nil, fmt.Errorf("health gate: %w", err)
	}
	if err := reloader.ValidateTools(tools); err != nil {
		return nil, fmt.Errorf("health gate: %w", err)
	}
	return tools, nil
}

// warnUnforwardedCapabilities logs a prominent warning when the child
// advertises prompts or resources: the v1 proxy forwards tools only, and the
// downstream superset envelope would otherwise silently answer empty
// prompt/resource lists for a child that actually has them.
func (u *Upstream) warnUnforwardedCapabilities(ctx context.Context, session *mcp.ClientSession) {
	result := session.InitializeResult()
	if result == nil || result.Capabilities == nil {
		return
	}
	if result.Capabilities.Prompts != nil || result.Capabilities.Resources != nil {
		u.logger.WarnContext(ctx,
			"CHILD ADVERTISES PROMPTS OR RESOURCES: the dev proxy forwards tools only (v1), "+
				"so prompts and resources will appear empty to the client",
			"prompts", result.Capabilities.Prompts != nil,
			"resources", result.Capabilities.Resources != nil)
	}
}

// fidelityGapMiddleware makes the v1 fidelity gaps loud on the proxy's
// stderr. roots/list must be rejected here: the capability opt-out only
// changes what is advertised, while the SDK's built-in handler would still
// answer an empty list. Sampling and elicitation already fail without
// handlers configured, so they are only logged.
func (u *Upstream) fidelityGapMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListRoots:
				u.logger.ErrorContext(ctx,
					"child requested roots/list; the dev proxy does not forward roots (v1 fidelity gap)")
				return nil, &jsonrpc.Error{
					Code:    jsonrpc.CodeMethodNotFound,
					Message: "mcp-devproxy does not support roots (v1 fidelity gap)",
				}
			case methodCreateMessage, methodElicit:
				u.logger.ErrorContext(ctx,
					"child used a server-to-client feature the dev proxy does not forward (v1 fidelity gap)",
					"method", method)
			}
			return next(ctx, method, req)
		}
	}
}

// commandTransport is the default TransportFactory: the configured argv with
// {{artifact}} substituted, run over stdio via mcp.CommandTransport with the
// child's stderr wired through. The child's lifetime belongs to the
// transport's Close ladder (stdin close, SIGTERM, SIGKILL) — deliberately
// not [exec.CommandContext], whose ctx kill would bypass that spec-correct
// shutdown.
func (u *Upstream) commandTransport(artifact string) (mcp.Transport, error) {
	argv := make([]string, len(u.argv))
	for i, field := range u.argv {
		argv[i] = strings.ReplaceAll(field, artifactToken, artifact)
	}

	//nolint:gosec,noctx // Running the developer-supplied child command is this adapter's purpose, and
	// its lifetime belongs to CommandTransport.Close's escalation ladder, not to a ctx kill.
	cmd := exec.Command(argv[0], argv[1:]...)
	// The child runs with the proxy's full environment, explicitly — the same
	// environment it would get if the developer ran it directly (the fidelity
	// rationale behind the stderr wiring below). A nil Env would inherit the
	// same way implicitly; the assignment makes inheritance a decision and
	// the one obvious seam for injecting per-cycle variables later.
	cmd.Env = os.Environ()
	cmd.Stderr = u.stderr
	return &mcp.CommandTransport{Command: cmd, TerminateDuration: u.terminate}, nil
}

// listTools collects the child's full tool list through the SDK's
// paginating iterator.
func listTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}
