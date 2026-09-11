package downstream

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// serverName identifies the proxy on the downstream hop when Options.Impl is
// nil.
const serverName = "mcp-devproxy"

// serverVersion is the version the proxy reports to the downstream client.
const serverVersion = "dev"

// Method strings for the downstream requests the receiving middleware
// observes, mirroring the SDK's unexported constants.
const (
	methodCallTool  = "tools/call"
	methodListTools = "tools/list"
	methodSetLevel  = "logging/setLevel"
)

// Options configures a Frontend for New.
type Options struct {
	// Impl identifies the proxy to the downstream client at initialize. Nil
	// selects the mcp-devproxy default.
	Impl *mcp.Implementation

	// Logger receives operational logs (stderr-bound in the proxy; stdout is
	// the protocol channel). Nil selects a no-op logger.
	Logger *slog.Logger
}

// Frontend is the mcp.Server-backed implementation of the reloader.Frontend
// port: the persistent, client-facing side of the dev proxy.
//
// The server is constructed with the superset capability envelope — tools,
// prompts, and resources with list_changed, plus logging — so a child gaining
// its first prompt or resource later stays reachable without a client
// reconnect: the downstream session's capabilities freeze at its one
// initialize, even though v1 forwards tools only.
type Frontend struct {
	server *mcp.Server
	logger *slog.Logger

	// mu is the adapter's only lock; it guards every field below. Lock
	// order: Reconcile holds mu across AddTool/RemoveTools/Sessions calls,
	// which take the SDK server's internal lock — safe because the SDK never
	// holds its lock while waiting on this adapter (it fetches the receiving
	// middleware handler under its lock and releases it before invoking).
	// The order is always mu, then the server lock; never the reverse.
	mu sync.Mutex

	// gen counts mutating Reconciles; it is the stale-view clock.
	gen uint64

	// advertised maps each advertised tool name to the fingerprint of its
	// current wire definition: the Reconcile diff baseline.
	advertised map[string]string

	// toolGen records the generation at which each advertised tool was last
	// added or replaced.
	toolGen map[string]uint64

	// tombstones records the generation at which a name was removed;
	// re-adding the name clears its tombstone.
	tombstones map[string]uint64

	// sessionGen records, per downstream session, the generation that
	// session had observed at its most recent successful tools/list. A
	// session absent from the map has observed generation 0: every tool
	// touched by any mutating Reconcile gates stale for it until its first
	// successful tools/list. The proxy cannot know what definitions such a
	// session holds; unknown never matches, mirroring the router's drain
	// gate.
	sessionGen map[*mcp.ServerSession]uint64

	// level is the last logging/setLevel observed from the downstream
	// client; empty until one is set.
	level mcp.LoggingLevel
}

// New constructs a Frontend serving the superset capability envelope and
// installs the stale-view-gating receiving middleware.
func New(options Options) (*Frontend, error) {
	impl := options.Impl
	if impl == nil {
		impl = &mcp.Implementation{Name: serverName, Version: serverVersion}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	frontend := &Frontend{
		logger:     logger,
		advertised: make(map[string]string),
		toolGen:    make(map[string]uint64),
		tombstones: make(map[string]uint64),
		sessionGen: make(map[*mcp.ServerSession]uint64),
	}
	frontend.server = mcp.NewServer(impl, &mcp.ServerOptions{
		Logger: logger,
		Capabilities: &mcp.ServerCapabilities{
			Tools:     &mcp.ToolCapabilities{ListChanged: true},
			Prompts:   &mcp.PromptCapabilities{ListChanged: true},
			Resources: &mcp.ResourceCapabilities{ListChanged: true},
			Logging:   &mcp.LoggingCapabilities{},
		},
	})
	frontend.server.AddReceivingMiddleware(frontend.middleware())
	return frontend, nil
}

// Reconcile makes the advertised tool set match tools, wiring each tool's
// handler to forward through call. Removed names go through one RemoveTools
// call; added and changed definitions each get a single replacing AddTool —
// the SDK replaces in place and coalesces the resulting tools/list_changed,
// so there is no Remove+Add window in which a tool transiently disappears.
// A no-op diff makes zero server calls and emits nothing. Definitions are
// validated before any mutation, so a bad one returns an error instead of
// reaching the SDK's AddTool panics, with the advertised set untouched.
//
// Each mutating Reconcile bumps the stale-view generation: a call from a
// session that has not re-listed since its target tool changed or vanished
// is answered with the friendly stale-reload error until that session lists
// again.
func (f *Frontend) Reconcile(tools []*mcp.Tool, call reloader.CallToolFunc) error {
	if err := reloader.ValidateTools(tools); err != nil {
		return fmt.Errorf("validate tools: %w", err)
	}
	fingerprints := make(map[string]string, len(tools))
	for _, tool := range tools {
		fingerprint, err := reloader.Fingerprint(tool)
		if err != nil {
			return fmt.Errorf("fingerprint tool %q: %w", tool.Name, err)
		}
		fingerprints[tool.Name] = fingerprint
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var removed []string
	for name := range f.advertised {
		if _, ok := fingerprints[name]; !ok {
			removed = append(removed, name)
		}
	}
	var changed []*mcp.Tool
	for _, tool := range tools {
		if f.advertised[tool.Name] != fingerprints[tool.Name] {
			changed = append(changed, tool)
		}
	}
	if len(removed) == 0 && len(changed) == 0 {
		return nil
	}

	f.gen++
	if len(removed) > 0 {
		f.server.RemoveTools(removed...)
		for _, name := range removed {
			f.tombstones[name] = f.gen
			delete(f.advertised, name)
			delete(f.toolGen, name)
		}
	}
	for _, tool := range changed {
		f.server.AddTool(tool, f.forwardHandler(call))
		f.advertised[tool.Name] = fingerprints[tool.Name]
		f.toolGen[tool.Name] = f.gen
		delete(f.tombstones, tool.Name)
	}
	f.pruneSessionsLocked()

	f.logger.Debug("reconciled the advertised tool set",
		"generation", f.gen, "added_or_changed", len(changed), "removed", len(removed))
	return nil
}

// Run serves the downstream session over transport until ctx is cancelled or
// the client closes the connection. The transport is injected for the same
// reason the template server drives its stdio command over provided streams:
// the seam is real, so tests connect in-memory transports and a networked
// downstream later slots in without touching this adapter's callers. In
// production the cli passes an IOTransport over the process streams — stdout
// is the protocol channel, and nothing in the proxy may write to it except
// the SDK transport.
func (f *Frontend) Run(ctx context.Context, transport mcp.Transport) error {
	if err := f.server.Run(ctx, transport); err != nil {
		return fmt.Errorf("serve downstream session: %w", err)
	}
	return nil
}

// Level returns the last logging/setLevel observed from the downstream
// client, or the empty level when none was ever set. The cli wires it to the
// upstream adapter's LevelProvider so each new child is replayed the level
// the client already chose.
func (f *Frontend) Level() mcp.LoggingLevel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.level
}

// Log forwards one child log message to every downstream session. The SDK
// drops the message per session when that session's client never set a
// logging level or set one above the message's, which is what keeps the
// advertised Logging capability honest. The cli wires this to the upstream
// adapter's LogHandler.
func (f *Frontend) Log(ctx context.Context, params *mcp.LoggingMessageParams) {
	for session := range f.server.Sessions() {
		if err := session.Log(ctx, params); err != nil {
			f.logger.WarnContext(ctx, "forwarding a child log message downstream failed", "error", err)
		}
	}
}

// forwardHandler wraps call as the raw, non-generic tool handler: the wire
// arguments are forwarded byte-for-byte with no validation or defaulting —
// the child does the validating — and Meta, including any progress token, is
// dropped by construction, per the reloader.CallToolFunc contract.
func (f *Frontend) forwardHandler(call reloader.CallToolFunc) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return call(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
	}
}

// middleware observes the downstream session: it records logging/setLevel
// for Level, gates stale tools/call requests, and records the generation
// each session observed at its last tools/list.
func (f *Frontend) middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodSetLevel:
				f.recordLevel(req)
			case methodCallTool:
				if result := f.gateStaleCall(ctx, req); result != nil {
					return result, nil
				}
			case methodListTools:
				return f.recordListGeneration(ctx, method, req, next)
			}
			return next(ctx, method, req)
		}
	}
}

// recordLevel stores the session's requested logging level for Level. The
// request still flows to the SDK, which records it on the session for its
// own per-session Log gating.
func (f *Frontend) recordLevel(req mcp.Request) {
	params, ok := req.GetParams().(*mcp.SetLoggingLevelParams)
	if !ok {
		return
	}
	f.mu.Lock()
	f.level = params.Level
	f.mu.Unlock()
}

// gateStaleCall answers a tools/call with the friendly stale-reload result
// when the target tool changed — or was removed — after the calling
// session's last tools/list, and returns nil when the call may dispatch.
// Interception must happen here rather than in a tool handler: a removed
// tool has no handler, and the SDK would answer its raw "unknown tool"
// protocol error instead of the friendly result the LLM can read.
//
// A session that has never listed has observed generation 0, so every tool
// touched by any mutating Reconcile gates stale for it until its first
// successful tools/list. The proxy cannot know what definitions such a
// session holds, and unknown never matches — the same conservative direction
// as the router's drain gate. MCP clients list before calling, so the gate
// upgrades only a blind call from a raw dispatch to the self-correcting
// stale-reload error.
func (f *Frontend) gateStaleCall(ctx context.Context, req mcp.Request) *mcp.CallToolResult {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok {
		return nil
	}
	session, _ := req.GetSession().(*mcp.ServerSession)

	f.mu.Lock()
	last := f.sessionGen[session]
	stale := f.tombstones[params.Name] > last || f.toolGen[params.Name] > last
	f.mu.Unlock()

	if !stale {
		return nil
	}
	f.logger.InfoContext(ctx,
		"gating a tool call issued against a stale tool list; calls flow once the session re-lists",
		"tool", params.Name)
	return reloader.StaleReloadResult(params.Name)
}

// recordListGeneration runs a tools/list and records the generation the
// session has now observed. The generation is read before the list executes,
// which is the conservative direction: a Reconcile landing mid-list at worst
// causes an extra friendly stale error, never a silent dispatch of old-shape
// arguments. Pagination is covered because every page re-records.
func (f *Frontend) recordListGeneration(
	ctx context.Context,
	method string,
	req mcp.Request,
	next mcp.MethodHandler,
) (mcp.Result, error) {
	f.mu.Lock()
	observed := f.gen
	f.mu.Unlock()

	result, err := next(ctx, method, req)
	if err != nil {
		return result, err
	}
	if session, ok := req.GetSession().(*mcp.ServerSession); ok {
		f.mu.Lock()
		if observed > f.sessionGen[session] {
			f.sessionGen[session] = observed
		}
		f.mu.Unlock()
	}
	return result, nil
}

// pruneSessionsLocked drops generation records for sessions the server no
// longer tracks. With a stdio downstream there is exactly one session for
// the proxy's life, so this is hygiene, not correctness. Callers hold mu.
func (f *Frontend) pruneSessionsLocked() {
	if len(f.sessionGen) == 0 {
		return
	}
	live := make(map[*mcp.ServerSession]bool, len(f.sessionGen))
	for session := range f.server.Sessions() {
		live[session] = true
	}
	for session := range f.sessionGen {
		if !live[session] {
			delete(f.sessionGen, session)
		}
	}
}
