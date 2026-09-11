// Integration tests: real go-sdk wiring over mcp.NewInMemoryTransports,
// driven through newProxy's production construction order — the seams struct
// injects only the process edges (watcher, builder, child transport,
// downstream transport), so the reloader core, both adapters, and the
// adapter-to-adapter logging passthrough under test are the real wiring.
// Whitebox: seams and config are unexported.
//
// The watcher and builder fakes are deliberate hand-rolled channel fakes, not
// mockery mocks: the reloader unit suite already proves those port contracts
// against mocks; here the ports are just faucets feeding the real SDK
// plumbing, which is what these tests exist to prove.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
	"github.com/meigma/template-mcp/tools/proxy/internal/upstream"
)

// waitTimeout bounds every asynchronous wait in this suite.
const waitTimeout = 5 * time.Second

// testDebounce keeps reload cycles fast: the core runs on the real clock
// here, so the production 300ms default would only slow the suite down.
const testDebounce = 10 * time.Millisecond

// channelWatcher implements reloader.Watcher over a test-fed channel.
type channelWatcher struct {
	events chan reloader.ChangeEvent
}

func (w *channelWatcher) Watch(context.Context) (<-chan reloader.ChangeEvent, error) {
	return w.events, nil
}

// channelBuilder implements reloader.Builder by handing out test-fed artifact
// paths. Holding the first artifact back is how the cold-start test keeps the
// downstream session serving before any child exists; Build honors ctx so the
// core's shutdown never stalls on a held-back build.
type channelBuilder struct {
	artifacts chan string
}

func (b *channelBuilder) Build(ctx context.Context) (reloader.BuildResult, error) {
	select {
	case artifact := <-b.artifacts:
		return reloader.BuildResult{Artifact: artifact}, nil
	case <-ctx.Done():
		return reloader.BuildResult{}, ctx.Err()
	}
}

// integrationChild is a real in-process mcp.Server standing in for one built
// child binary. The production upstream adapter connects to it through the
// transport seam and runs its real initialize and health gate against it; its
// tool handlers record the wire argument bytes and answer with canned,
// child-identifying text.
type integrationChild struct {
	name    string
	server  *mcp.Server
	session atomic.Pointer[mcp.ServerSession]

	// calls records each forwarded call's raw argument bytes, as received.
	calls chan string

	// setLevel records every logging/setLevel the child receives — the
	// observable proof that the upstream adapter replayed the downstream
	// client's level to a freshly started child.
	setLevel chan mcp.LoggingLevel
}

func newIntegrationChild(name string) *integrationChild {
	child := &integrationChild{
		name:     name,
		calls:    make(chan string, 16),
		setLevel: make(chan mcp.LoggingLevel, 16),
	}
	child.server = mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	child.server.AddReceivingMiddleware(child.recordSetLevel())
	return child
}

// addTool registers one raw-handler tool: the handler records the wire
// arguments byte-for-byte and returns text identifying both child and tool.
func (c *integrationChild) addTool(name string, schema map[string]any) {
	c.server.AddTool(
		&mcp.Tool{Name: name, InputSchema: schema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			c.calls <- string(req.Params.Arguments)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok-" + c.name + "-" + name}},
			}, nil
		},
	)
}

// serverSession returns the child's server-side session captured when the
// upstream adapter connected to it.
func (c *integrationChild) serverSession(t *testing.T) *mcp.ServerSession {
	t.Helper()

	session := c.session.Load()
	require.NotNil(t, session, "expected the upstream adapter to have connected this child")
	return session
}

// recordSetLevel observes logging/setLevel on the child and feeds setLevel.
func (c *integrationChild) recordSetLevel() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "logging/setLevel" {
				if params, ok := req.GetParams().(*mcp.SetLoggingLevelParams); ok {
					c.setLevel <- params.Level
				}
			}
			return next(ctx, method, req)
		}
	}
}

// integrationHarness runs one fully wired proxy: a real mcp.Client (standing
// in for Claude) over an in-memory downstream transport, fake children
// injected through the upstream transport seam, and the watch/build ports fed
// by channels.
type integrationHarness struct {
	events    chan reloader.ChangeEvent
	artifacts chan string

	// children maps artifact paths to the fake child each cycle connects.
	// It is written only by the test goroutine, strictly before the channel
	// send (artifact or change event) that lets the core's cycle goroutine
	// reach the transport factory — the channel handoff is the
	// happens-before edge that publishes the write.
	children map[string]*integrationChild

	client      *mcp.ClientSession
	listChanged chan struct{}
	logs        chan *mcp.LoggingMessageParams
	runErr      chan error

	// ctx and cancel bound proxy.run and every connection the harness makes;
	// shutdown's cancel is the clean-exit trigger under test.
	ctx    context.Context //nolint:containedctx // A test harness's lifetime is the context's lifetime.
	cancel context.CancelFunc
	proxy  *proxy

	// down records that shutdown already ran: the E2E test asserts shutdown
	// explicitly as its final phase, and the registered cleanup must not wait
	// on the once-only runErr channel a second time.
	down bool
}

// newIntegrationHarness wires the proxy with the integration fakes injected
// at every process edge: channel-fed watcher and builder, in-memory children
// behind the upstream transport seam.
func newIntegrationHarness(t *testing.T) *integrationHarness {
	t.Helper()

	h := &integrationHarness{
		events:      make(chan reloader.ChangeEvent, 1),
		artifacts:   make(chan string, 1),
		children:    make(map[string]*integrationChild),
		listChanged: make(chan struct{}, 16),
		logs:        make(chan *mcp.LoggingMessageParams, 16),
		runErr:      make(chan error, 1),
	}

	h.ctx, h.cancel = context.WithCancel(t.Context())

	h.start(t, config{debounce: testDebounce}, seams{
		watcher:        &channelWatcher{events: h.events},
		builder:        &channelBuilder{artifacts: h.artifacts},
		childTransport: h.childTransport(h.ctx),
	}, io.Discard, discardLogger())
	return h
}

// start wires the proxy through newProxy (the production construction order)
// with the downstream in-memory transport injected into s, starts proxy.run,
// and connects the fake Claude client. The connect ordering is safe: proxy.run
// connects the server side first, and the in-memory transport blocks the
// client's initialize write until the server side reads. The E2E suite reuses
// it with the production seams (only the downstream transport injected) and a
// real config.
func (h *integrationHarness) start(
	t *testing.T,
	cfg config,
	s seams,
	errOut io.Writer,
	logger *slog.Logger,
) {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	s.downstreamTransport = serverTransport
	p, err := newProxy(cfg, strings.NewReader(""), io.Discard, errOut, logger, s)
	require.NoError(t, err, "wire the proxy through newProxy")
	h.proxy = p

	go func() { h.runErr <- p.run(h.ctx) }()

	client := mcp.NewClient(
		&mcp.Implementation{Name: "fake-claude", Version: "0.0.1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				select {
				case h.listChanged <- struct{}{}:
				default:
				}
			},
			LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
				select {
				case h.logs <- req.Params:
				default:
				}
			},
		},
	)
	session, err := client.Connect(h.ctx, clientTransport, nil)
	require.NoError(t, err, "connect the fake Claude client to the proxy's downstream server")
	h.client = session

	t.Cleanup(func() { h.shutdown(t) })
}

// childTransport is the upstream.TransportFactory: it connects the registered
// fake child's server to one end of an in-memory pair (servers must be
// connected before clients) and hands the production upstream adapter the
// other end, so the adapter's real connect, initialize, level replay, and
// health gate all run against the fake child.
func (h *integrationHarness) childTransport(ctx context.Context) upstream.TransportFactory {
	return func(artifact string) (mcp.Transport, error) {
		child, ok := h.children[artifact]
		if !ok {
			return nil, fmt.Errorf("no fake child registered for artifact %q", artifact)
		}
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		session, err := child.server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, fmt.Errorf("connect fake child %q: %w", child.name, err)
		}
		child.session.Store(session)
		return clientTransport, nil
	}
}

// firstArtifact is the artifact path every test's cold-start build produces.
const firstArtifact = "artifact-1"

// serveFirstChild releases the held-back cold-start build to child and waits
// for the resulting tools/list_changed; the proxy is SERVING child when it
// returns.
func (h *integrationHarness) serveFirstChild(t *testing.T, child *integrationChild) {
	t.Helper()

	h.children[firstArtifact] = child
	h.feedArtifact(t, firstArtifact)
	h.awaitListChanged(t)
}

// triggerReload registers the next cycle's child, fires a watcher event, and
// releases the artifact, driving one full debounce, build, health-gate, swap,
// reconcile cycle through the production core.
func (h *integrationHarness) triggerReload(t *testing.T, artifact string, child *integrationChild) {
	t.Helper()

	h.children[artifact] = child
	h.drainListChanged()
	select {
	case h.events <- reloader.ChangeEvent{Path: "internal/server/tools.go"}:
	case <-time.After(waitTimeout):
		t.Fatal("expected the core to consume the watcher event")
	}
	h.feedArtifact(t, artifact)
}

// feedArtifact releases one held-back build result to the core.
func (h *integrationHarness) feedArtifact(t *testing.T, artifact string) {
	t.Helper()

	select {
	case h.artifacts <- artifact:
	case <-time.After(waitTimeout):
		t.Fatal("expected the core's build port to consume the artifact")
	}
}

// awaitListChanged waits for one tools/list_changed to reach the fake Claude
// client's ToolListChangedHandler.
func (h *integrationHarness) awaitListChanged(t *testing.T) {
	t.Helper()

	h.awaitListChangedWithin(t, waitTimeout)
}

// awaitListChangedWithin is awaitListChanged with an explicit budget: the E2E
// cold start passes a longer one because its first real go build may compile
// the SDK from a cold build cache.
func (h *integrationHarness) awaitListChangedWithin(t *testing.T, timeout time.Duration) {
	t.Helper()

	select {
	case <-h.listChanged:
	case <-time.After(timeout):
		t.Fatal("expected the client's ToolListChangedHandler to fire")
	}
}

// drainListChanged discards already-delivered notifications so the next
// awaitListChanged observes only the upcoming reload's.
func (h *integrationHarness) drainListChanged() {
	for {
		select {
		case <-h.listChanged:
		default:
			return
		}
	}
}

// listNames lists tools as the client and returns their names. Listing is
// also what opens the downstream stale-view gate, exactly as Claude Code
// re-lists on a tools/list_changed.
func (h *integrationHarness) listNames(t *testing.T) []string {
	t.Helper()

	result, err := h.client.ListTools(t.Context(), nil)
	require.NoError(t, err, "list tools from the client")
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// callTool issues one tools/call from the client with empty arguments.
func (h *integrationHarness) callTool(t *testing.T, name string) *mcp.CallToolResult {
	t.Helper()

	result, err := h.client.CallTool(
		t.Context(),
		&mcp.CallToolParams{Name: name, Arguments: map[string]any{}},
	)
	require.NoError(t, err, "expected a tool result for %q, not a protocol error", name)
	return result
}

// shutdown is the clean-exit assertion every test ends with: cancelling the
// run context must bring the downstream session and the core (including every
// child it owns) down, and proxy.run must classify that as a clean nil exit.
// It is idempotent — the E2E test asserts it explicitly as its final phase
// while it stays registered as a cleanup — and it releases the proxy's
// resources afterward, exactly as production's deferred close does.
func (h *integrationHarness) shutdown(t *testing.T) {
	t.Helper()

	if h.down {
		return
	}
	h.down = true

	h.cancel()
	select {
	case err := <-h.runErr:
		require.NoError(t, err, "expected cancellation to classify as a clean shutdown")
	case <-time.After(waitTimeout):
		t.Fatal("expected proxy.run to return after cancellation")
	}
	_ = h.client.Close()
	require.NoError(t, h.proxy.close(), "release the proxy's resources after run returned")
}

// objectSchema returns a fresh minimal valid input schema.
func objectSchema() map[string]any {
	return map[string]any{"type": "object"}
}

// propertySchema returns an object schema with one string property, so two
// same-named tools built from different properties fingerprint as changed.
func propertySchema(property string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{property: map[string]any{"type": "string"}},
	}
}

// requireChildResult asserts result is the identified child tool's canned
// success text — the proof of which child actually served the call.
func requireChildResult(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()

	require.False(t, result.IsError, "expected the call to dispatch to the child: %s", callText(t, result))
	assert.Equal(t, want, callText(t, result), "expected the identified child's canned result")
}

// requireStaleResult asserts result is the friendly stale-reload error the
// LLM can read and self-correct from.
func requireStaleResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()

	require.True(t, result.IsError, "expected the friendly stale-reload error result")
	assert.Contains(t, callText(t, result), "changed by dev reload",
		"expected the stale-reload message the LLM can read")
}

// callText extracts the first text content of a tool result.
func callText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) == 0 {
		return ""
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content in the tool result")
	return text.Text
}

// TestIntegrationColdStart proves the design's cold-start contract: the
// client connects and lists before any child exists (empty tool set, session
// served instantly), and the first successful child triggers a normal
// reconcile — the client's ToolListChangedHandler fires, the re-listed set
// carries the tool, and the call reaches the child.
//
// It also records the empirical answer to open question §10.4 (upstream init
// params): the upstream adapter sends a fresh, proxy-identity initialize —
// nothing replays the downstream client's init params ("fake-claude") — and
// the child accepts it: the handshake, level replay, and health gate all
// succeeded against it, and the child observed "mcp-devproxy" as its client.
func TestIntegrationColdStart(t *testing.T) {
	t.Parallel()

	h := newIntegrationHarness(t)

	assert.Empty(t, h.listNames(t),
		"expected the session to serve an empty tool list before the first child exists")

	child := newIntegrationChild("childA")
	child.addTool("alpha", objectSchema())
	h.serveFirstChild(t, child)

	assert.Equal(t, []string{"alpha"}, h.listNames(t),
		"expected the first child's tool to be advertised after list_changed")
	requireChildResult(t, h.callTool(t, "alpha"), "ok-childA-alpha")

	initParams := child.serverSession(t).InitializeParams()
	require.NotNil(t, initParams, "expected the child to have observed an initialize")
	require.NotNil(t, initParams.ClientInfo, "expected client info in the child's initialize")
	assert.Equal(t, "mcp-devproxy", initParams.ClientInfo.Name,
		"expected a fresh proxy-identity initialize, not a replay of the downstream client's (§10.4)")
}

// TestIntegrationReloadAndStaleViewGating drives a simulated reload end to
// end: the swapped-in child's new tool is advertised via list_changed and
// callable, while a call issued from the client's stale pre-reload view gets
// the friendly stale-reload error until the client re-lists — at which point
// the same call dispatches to the new child.
func TestIntegrationReloadAndStaleViewGating(t *testing.T) {
	t.Parallel()

	h := newIntegrationHarness(t)
	childA := newIntegrationChild("childA")
	childA.addTool("alpha", objectSchema())
	h.serveFirstChild(t, childA)
	require.Equal(t, []string{"alpha"}, h.listNames(t), "expected child A's tool set to be served")

	childB := newIntegrationChild("childB")
	childB.addTool("alpha", propertySchema("city")) // same name, changed schema
	childB.addTool("secret", objectSchema())        // the newly added tool
	h.triggerReload(t, "artifact-2", childB)
	h.awaitListChanged(t)

	// The client has not re-listed: its cached "alpha" definition predates
	// the swap, so the call is gated with the friendly error instead of
	// silently running old-shape arguments on new code.
	requireStaleResult(t, h.callTool(t, "alpha"))

	// Re-listing (what Claude Code does on list_changed) opens the gate.
	assert.ElementsMatch(t, []string{"alpha", "secret"}, h.listNames(t),
		"expected the re-listed set to carry the new child's tools")
	requireChildResult(t, h.callTool(t, "secret"), "ok-childB-secret")
	requireChildResult(t, h.callTool(t, "alpha"), "ok-childB-alpha")
}

// TestIntegrationArgumentBytePassthrough proves the full forwarding chain —
// downstream raw handler, core router, upstream client — never re-encodes,
// validates, or defaults arguments. The payload is crafted so any generic,
// schema-aware path WOULD mutate it: unusual key order and a trailing-zero
// number literal (re-encoding normalizes both), a field absent from the
// child's schema (validation rejects or strips it), and the schema's
// defaulted "limit" property omitted (defaulting would inject it). String
// equality, never JSONEq: re-encoding is exactly the bug class under test.
func TestIntegrationArgumentBytePassthrough(t *testing.T) {
	t.Parallel()

	h := newIntegrationHarness(t)
	child := newIntegrationChild("childA")
	child.addTool("echo", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer", "default": 10},
		},
	})
	h.serveFirstChild(t, child)
	h.listNames(t)

	sent := `{"zz":1.50,"a":{"k":[2,1]},"unknown_field":"-"}`
	result, err := h.client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: json.RawMessage(sent),
	})
	require.NoError(t, err, "call the tool with raw argument bytes")
	requireChildResult(t, result, "ok-childA-echo")

	select {
	case got := <-child.calls:
		assert.Equal(t, sent, got,
			"expected the child to receive the exact original bytes: no validation, defaulting, or re-encoding")
	case <-time.After(waitTimeout):
		t.Fatal("expected the call to reach the child's handler")
	}
}

// TestIntegrationLoggingPassthrough proves the cli's adapter-to-adapter
// logging wires, which deliberately bypass the core: the downstream
// frontend's last-known logging/setLevel feeds each new child at Start
// (frontend.Level → upstream LevelProvider), and a child's log notification
// flows to the downstream client (upstream LogHandler → frontend.Log).
func TestIntegrationLoggingPassthrough(t *testing.T) {
	t.Parallel()

	h := newIntegrationHarness(t)
	childA := newIntegrationChild("childA")
	childA.addTool("alpha", objectSchema())
	h.serveFirstChild(t, childA)

	require.NoError(t,
		h.client.SetLoggingLevel(t.Context(), &mcp.SetLoggingLevelParams{Level: "debug"}),
		"set the logging level from the client")

	childB := newIntegrationChild("childB")
	childB.addTool("alpha", objectSchema())
	h.triggerReload(t, "artifact-2", childB)

	select {
	case level := <-childB.setLevel:
		assert.Equal(t, mcp.LoggingLevel("debug"), level,
			"expected the client's level to be replayed to the freshly started child")
	case <-time.After(waitTimeout):
		t.Fatal("expected the new child to receive logging/setLevel during Start")
	}

	require.NoError(t,
		childB.serverSession(t).Log(t.Context(), &mcp.LoggingMessageParams{Level: "error", Data: "from-childB"}),
		"log from the new child")
	select {
	case params := <-h.logs:
		assert.Equal(t, "from-childB", params.Data,
			"expected the child's log message to reach the downstream client's handler")
	case <-time.After(waitTimeout):
		t.Fatal("expected the child's log message to be forwarded downstream")
	}
}
