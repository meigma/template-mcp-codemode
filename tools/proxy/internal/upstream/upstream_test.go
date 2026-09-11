package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
	"github.com/meigma/template-mcp/tools/proxy/internal/upstream"
)

// waitTimeout bounds every asynchronous wait in this suite.
const waitTimeout = 5 * time.Second

// tick is the polling interval for Eventually/Never-style waits.
const tick = 10 * time.Millisecond

// logRecorder is a [slog.Handler] that captures log messages for assertions.
type logRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the message with its attrs folded in as key=value pairs, so
// assertions can match attribute values (such as the fidelity-gap method).
func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	message := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		message += " " + attr.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, message)
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *logRecorder) WithGroup(string) slog.Handler { return r }

func (r *logRecorder) contains(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, message := range r.messages {
		if strings.Contains(message, substr) {
			return true
		}
	}
	return false
}

// fakeChild is a real in-process mcp.Server standing in for the built child
// binary; the upstream adapter connects to it through the transport seam.
type fakeChild struct {
	server  *mcp.Server
	session atomic.Pointer[mcp.ServerSession]

	// conns captures the child's raw side of the wire at connect, letting
	// tests inject protocol messages the SDK's server API refuses to send.
	conns chan mcp.Connection
}

func newFakeChild(toolNames ...string) *fakeChild {
	child := &fakeChild{
		server: mcp.NewServer(&mcp.Implementation{Name: "fake-child", Version: "0.0.1"}, nil),
		conns:  make(chan mcp.Connection, 1),
	}
	for _, name := range toolNames {
		child.addTool(name)
	}
	return child
}

// addTool registers a valid passthrough tool whose result identifies it.
func (c *fakeChild) addTool(name string) {
	c.server.AddTool(
		&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok-" + name}}}, nil
		},
	)
}

// factory returns a TransportFactory that connects the fake child server to
// one end of an in-memory transport pair and hands the adapter the other
// end (servers must be connected before clients).
func (c *fakeChild) factory(t *testing.T) upstream.TransportFactory {
	return func(string) (mcp.Transport, error) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		tapped := &tappingTransport{transport: serverTransport, conns: c.conns}
		session, err := c.server.Connect(t.Context(), tapped, nil)
		if err != nil {
			return nil, err
		}
		c.session.Store(session)
		return clientTransport, nil
	}
}

// tappingTransport hands the wrapped transport's Connection through untouched
// while keeping a reference for the test.
type tappingTransport struct {
	transport mcp.Transport
	conns     chan<- mcp.Connection
}

func (t *tappingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.transport.Connect(ctx)
	if err == nil {
		select {
		case t.conns <- conn:
		default:
		}
	}
	return conn, err
}

// rawConnection returns the fake child's side of the wire captured at
// connect. [mcp.Connection.Write] is documented as safe to call concurrently,
// so injecting a message races neither the child server's own writes nor its
// read loop.
func (c *fakeChild) rawConnection(t *testing.T) mcp.Connection {
	t.Helper()

	select {
	case conn := <-c.conns:
		return conn
	default:
		t.Fatal("expected the transport factory to have captured the child's connection")
		return nil
	}
}

// serverSession returns the fake child's server-side session captured by the
// factory during Start.
func (c *fakeChild) serverSession(t *testing.T) *mcp.ServerSession {
	t.Helper()

	session := c.session.Load()
	require.NotNil(t, session, "expected the transport factory to have connected the fake child")
	return session
}

// testContext bundles the fake child, the adapter under test, and its
// captured logs.
type testContext struct {
	child *fakeChild
	logs  *logRecorder
	up    *upstream.Upstream
}

// newTestContext wires options to the fake child's transport seam and a log
// recorder, then constructs the adapter.
func newTestContext(t *testing.T, child *fakeChild, options upstream.Options) *testContext {
	t.Helper()

	logs := &logRecorder{}
	options.Transport = child.factory(t)
	options.Logger = slog.New(logs)

	up, err := upstream.New(options)
	require.NoError(t, err, "construct upstream")
	return &testContext{child: child, logs: logs, up: up}
}

// start runs Start against the fake child and registers cleanup.
func (tc *testContext) start(t *testing.T) reloader.ChildSession {
	t.Helper()

	session, err := tc.up.Start(t.Context(), "unused-artifact")
	require.NoError(t, err, "start child")
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// awaitSnapshot polls ToolsChanged until a snapshot containing wantTool
// arrives, keeping the most recent one.
func awaitSnapshot(t *testing.T, session reloader.ChildSession, wantTool string) []*mcp.Tool {
	t.Helper()

	var snapshot []*mcp.Tool
	require.Eventually(t, func() bool {
		select {
		case latest := <-session.ToolsChanged():
			snapshot = latest
		default:
		}
		return slices.Contains(toolNames(snapshot), wantTool)
	}, waitTimeout, tick, "expected a validated snapshot containing %q on ToolsChanged", wantTool)
	return snapshot
}

// serveToolList intercepts tools/list on the fake child to serve definitions
// its own AddTool validation would refuse to register.
func serveToolList(tools []*mcp.Tool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				return &mcp.ListToolsResult{Tools: tools}, nil
			}
			return next(ctx, method, req)
		}
	}
}

// requireChildKilled asserts the fake child's session was closed — the
// health gate must kill a half-started child it rejects.
func requireChildKilled(t *testing.T, child *fakeChild) {
	t.Helper()

	session := child.serverSession(t)
	closed := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(waitTimeout):
		t.Fatal("expected the half-started child session to be closed after a gate failure")
	}
}

func TestUpstreamStartHappyPath(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t, newFakeChild("alpha", "beta"), upstream.Options{})
	session := tc.start(t)

	assert.ElementsMatch(t, []string{"alpha", "beta"}, toolNames(session.Tools()),
		"expected the health-gate snapshot to carry the child's tools")

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "alpha"})
	require.NoError(t, err, "forward a tool call to the child")
	require.Len(t, result.Content, 1, "expected the child's result content")
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content from the fake child")
	assert.Equal(t, "ok-alpha", text.Text, "expected the call to reach the child's handler")
}

func TestUpstreamStartHealthGateFailures(t *testing.T) {
	t.Parallel()

	objectSchema := map[string]any{"type": "object"}

	tests := []struct {
		name    string
		tools   []*mcp.Tool
		wantErr string
	}{
		{
			name:    "missing input schema",
			tools:   []*mcp.Tool{{Name: "broken"}},
			wantErr: "missing input schema",
		},
		{
			name:    "non-object input schema",
			tools:   []*mcp.Tool{{Name: "broken", InputSchema: map[string]any{"type": "array"}}},
			wantErr: `not "object"`,
		},
		{
			name:    "invalid tool name",
			tools:   []*mcp.Tool{{Name: "bad name", InputSchema: objectSchema}},
			wantErr: "invalid character",
		},
		{
			name: "duplicate tool names",
			tools: []*mcp.Tool{
				{Name: "dup", InputSchema: objectSchema},
				{Name: "dup", InputSchema: objectSchema},
			},
			wantErr: "duplicate tool name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			child := newFakeChild()
			child.server.AddReceivingMiddleware(serveToolList(tt.tools))
			tc := newTestContext(t, child, upstream.Options{})

			_, err := tc.up.Start(t.Context(), "unused-artifact")

			require.Error(t, err, "expected the health gate to reject the child")
			require.ErrorContains(t, err, "health gate", "expected the gate to identify itself")
			require.ErrorContains(t, err, tt.wantErr, "expected the rejection to say why")
			requireChildKilled(t, child)
		})
	}
}

func TestUpstreamStartHealthGateTimeout(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	child.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				select {
				case <-ctx.Done():
				case <-release:
				}
			}
			return next(ctx, method, req)
		}
	})
	tc := newTestContext(t, child, upstream.Options{HealthTimeout: 100 * time.Millisecond})

	start := time.Now()
	_, err := tc.up.Start(t.Context(), "unused-artifact")
	elapsed := time.Since(start)

	require.Error(t, err, "expected the health gate to time out on an unresponsive child")
	require.ErrorContains(t, err, "health gate", "expected the gate to identify itself")
	assert.Less(t, elapsed, 3*time.Second,
		"expected Start to fail within the health timeout, not hang on the child")
}

// TestUpstreamStartLoggingReplayTimeout proves the logging/setLevel replay
// is bounded by the health timeout: a child that never answers it must not
// stall Start, and because the replay is optional the bounded failure is
// logged and ignored rather than failing the gate — leaving a session that
// still serves.
func TestUpstreamStartLoggingReplayTimeout(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	child.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "logging/setLevel" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, method, req)
		}
	})
	tc := newTestContext(t, child, upstream.Options{
		HealthTimeout: 100 * time.Millisecond,
		LevelProvider: func() mcp.LoggingLevel { return "warning" },
	})

	// Start runs in a goroutine so a lost replay bound fails this test
	// crisply at waitTimeout instead of hanging it until the runner's
	// timeout: the middleware above only unblocks when its ctx is cancelled,
	// and without the bound that is the test's own context.
	type startResult struct {
		session reloader.ChildSession
		elapsed time.Duration
		err     error
	}
	results := make(chan startResult, 1)
	go func() {
		start := time.Now()
		session, err := tc.up.Start(t.Context(), "unused-artifact")
		results <- startResult{session: session, elapsed: time.Since(start), err: err}
	}()

	var result startResult
	select {
	case result = <-results:
	case <-time.After(waitTimeout):
		t.Fatal("expected the replay to be bounded by the health timeout — Start is hung on the unanswered setLevel")
	}

	require.NoError(t, result.err, "expected Start to succeed: the level replay is optional")
	session := result.session
	t.Cleanup(func() { _ = session.Close() })
	assert.Less(t, result.elapsed, 3*time.Second,
		"expected the replay to be bounded by the health timeout, not hang Start")
	assert.True(t, tc.logs.contains("replaying the logging level"),
		"expected the bounded replay failure to be logged and ignored")

	// The ignored failure must leave a functional session behind, not just a
	// non-error: the health-gate snapshot carries the tools and calls reach
	// the child.
	assert.ElementsMatch(t, []string{"alpha"}, toolNames(session.Tools()),
		"expected the session returned after the ignored replay failure to carry the child's tools")
	_, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "alpha"})
	require.NoError(t, err, "expected the child to still serve tool calls after the ignored replay failure")
}

// hangingTransport simulates a child that starts but never completes the MCP
// handshake: its connection accepts writes and never delivers a response.
type hangingTransport struct {
	conn *hangingConnection
}

func (t *hangingTransport) Connect(context.Context) (mcp.Connection, error) {
	return t.conn, nil
}

// hangingConnection blocks every Read until the connection is closed, so the
// initialize request outlives any patience the caller grants it.
type hangingConnection struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *hangingConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	}
}

func (c *hangingConnection) Write(context.Context, jsonrpc.Message) error { return nil }

func (c *hangingConnection) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *hangingConnection) SessionID() string { return "" }

// TestUpstreamStartConnectTimeout proves the connect+initialize step is
// bounded by the health timeout: a child that hangs before completing
// initialize must not stall Start, and the half-started session must be torn
// down — the SDK closes it when initialize fails, which for the real
// CommandTransport runs the SIGTERM/SIGKILL ladder, leaving no orphan.
func TestUpstreamStartConnectTimeout(t *testing.T) {
	t.Parallel()

	conn := &hangingConnection{closed: make(chan struct{})}
	up, err := upstream.New(upstream.Options{
		HealthTimeout: 100 * time.Millisecond,
		Transport:     func(string) (mcp.Transport, error) { return &hangingTransport{conn: conn}, nil },
	})
	require.NoError(t, err, "construct upstream")

	start := time.Now()
	_, err = up.Start(t.Context(), "unused-artifact")
	elapsed := time.Since(start)

	require.Error(t, err, "expected Start to fail when the child hangs during initialize")
	require.ErrorContains(t, err, "connect to child", "expected the connect step to identify itself")
	assert.Less(t, elapsed, 3*time.Second,
		"expected Start to fail within the health timeout, not hang on initialize")
	require.Eventually(t, func() bool {
		select {
		case <-conn.closed:
			return true
		default:
			return false
		}
	}, waitTimeout, tick,
		"expected the hung child's connection to be torn down — no live session left behind")
}

func TestUpstreamChildRuntimeToolChange(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	tc := newTestContext(t, child, upstream.Options{})
	session := tc.start(t)

	child.addTool("gamma")
	snapshot := awaitSnapshot(t, session, "gamma")
	assert.ElementsMatch(t, []string{"alpha", "gamma"}, toolNames(snapshot),
		"expected the re-listed snapshot to carry the full new set")

	// A burst with no reader: latest-wins publishing must absorb the extra
	// snapshot instead of wedging the child's notification handling.
	child.addTool("delta")
	child.addTool("epsilon")
	snapshot = awaitSnapshot(t, session, "epsilon")
	assert.Contains(t, toolNames(snapshot), "delta",
		"expected the final snapshot to include every tool from the burst")
}

// TestUpstreamRuntimeRelistFailureSignalsDone covers the §5 row for a failed
// runtime re-list: when a serving child emits tools/list_changed but the
// re-list errors or validates invalid, the child itself declared the
// advertised set stale with no trusted replacement, so the adapter must
// treat it as child death — Done fires and the core's crash supervision
// restarts it — never keep routing against the untrusted old set.
func TestUpstreamRuntimeRelistFailureSignalsDone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		respond func() (mcp.Result, error)
	}{
		{
			name: "re-list returns an error",
			respond: func() (mcp.Result, error) {
				return nil, errors.New("child re-list failed")
			},
		},
		{
			name: "re-list serves an invalid definition",
			respond: func() (mcp.Result, error) {
				return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "broken"}}}, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var broken atomic.Bool
			child := newFakeChild("alpha")
			child.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "tools/list" && broken.Load() {
						return tt.respond()
					}
					return next(ctx, method, req)
				}
			})
			tc := newTestContext(t, child, upstream.Options{})
			session := tc.start(t)

			broken.Store(true)
			child.addTool("gamma")

			require.Eventually(t, func() bool {
				select {
				case <-session.Done():
					return true
				default:
					return false
				}
			}, waitTimeout, tick,
				"expected a failed runtime re-list to close the child session and fire Done")
			assert.True(t, tc.logs.contains("unhealthy"),
				"expected the unhealthy treatment to be loud in the logs")
			assert.NoError(t, session.Close(),
				"expected a Close after the unhealthy close to stay safe")
		})
	}
}

func TestUpstreamChildDeathClosesDone(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	tc := newTestContext(t, child, upstream.Options{})
	session := tc.start(t)

	require.NoError(t, child.serverSession(t).Close(), "kill the fake child")

	require.Eventually(t, func() bool {
		select {
		case <-session.Done():
			return true
		default:
			return false
		}
	}, waitTimeout, tick, "expected Done to close when the child dies unexpectedly")
}

func TestUpstreamCloseDoesNotSignalDone(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	tc := newTestContext(t, child, upstream.Options{})
	session := tc.start(t)

	require.NoError(t, session.Close(), "close the child session")

	assert.Never(t, func() bool {
		select {
		case <-session.Done():
			return true
		default:
			return false
		}
	}, 300*time.Millisecond, tick, "expected an intentional Close never to report a crash on Done")
}

func TestUpstreamRootsListRejected(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	tc := newTestContext(t, child, upstream.Options{})
	tc.start(t)

	_, err := child.serverSession(t).ListRoots(t.Context(), nil)

	require.Error(t, err, "expected roots/list to be rejected, not answered with an empty list")
	require.ErrorContains(t, err, "does not support roots", "expected the explicit fidelity-gap error")
	assert.True(t, tc.logs.contains("roots/list"), "expected a loud log about the roots fidelity gap")
}

// TestUpstreamSamplingRejectedLoudly covers the sampling half of the §5
// fidelity-gap row: the proxy configures no sampling handler, so the child's
// sampling/createMessage gets an error back — and the gap must be loud on the
// proxy's logs, never silent.
func TestUpstreamSamplingRejectedLoudly(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	tc := newTestContext(t, child, upstream.Options{})
	tc.start(t)

	_, err := child.serverSession(t).CreateMessage(t.Context(), &mcp.CreateMessageParams{})

	require.Error(t, err, "expected sampling to fail: the proxy configures no sampling handler")
	require.ErrorContains(t, err, "does not support", "expected the unsupported-feature error to reach the child")
	assert.True(t, tc.logs.contains("sampling/createMessage"),
		"expected a loud log naming the sampling fidelity gap")
}

// TestUpstreamElicitationRejectedLoudly covers the elicitation half of the §5
// fidelity-gap row. A well-behaved go-sdk child cannot even issue the request:
// ServerSession.Elicit refuses locally because the proxy advertises no
// elicitation capability. The loud-log contract is about the wire, so the
// request is injected raw on the child's connection — exactly what a non-SDK
// or ill-behaved child would send.
func TestUpstreamElicitationRejectedLoudly(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	tc := newTestContext(t, child, upstream.Options{})
	session := tc.start(t)

	id, err := jsonrpc.MakeID("fidelity-gap-elicit")
	require.NoError(t, err, "make the injected request id")
	require.NoError(t, child.rawConnection(t).Write(t.Context(), &jsonrpc.Request{
		ID:     id,
		Method: "elicitation/create",
		Params: json.RawMessage(`{"message":"pick one","requestedSchema":{"type":"object"}}`),
	}), "inject the raw elicitation request from the child's side of the wire")

	require.Eventually(t, func() bool { return tc.logs.contains("elicitation/create") },
		waitTimeout, tick, "expected a loud log naming the elicitation fidelity gap")

	// The error response to the injected request must not damage the session:
	// the child keeps serving tool calls afterwards.
	_, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "alpha"})
	assert.NoError(t, err, "expected the session to survive the rejected elicitation")
}

func TestUpstreamLoggingPassthrough(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	received := make(chan *mcp.LoggingMessageParams, 8)
	tc := newTestContext(t, child, upstream.Options{
		LogHandler:    func(_ context.Context, params *mcp.LoggingMessageParams) { received <- params },
		LevelProvider: func() mcp.LoggingLevel { return "warning" },
	})
	tc.start(t)

	session := child.serverSession(t)
	require.NoError(t, session.Log(t.Context(), &mcp.LoggingMessageParams{Level: "info", Data: "below"}),
		"log below the replayed level")
	require.NoError(t, session.Log(t.Context(), &mcp.LoggingMessageParams{Level: "error", Data: "above"}),
		"log above the replayed level")

	select {
	case params := <-received:
		assert.Equal(t, mcp.LoggingLevel("error"), params.Level,
			"expected only the message at or above the replayed level to be forwarded")
	case <-time.After(waitTimeout):
		t.Fatal("expected the error-level message to reach the log handler — was the level replayed?")
	}
	assert.Empty(t, received,
		"expected the info-level message to be gated by the replayed logging level")
}

func TestUpstreamWarnsOnUnforwardedCapabilities(t *testing.T) {
	t.Parallel()

	child := newFakeChild("alpha")
	child.server.AddPrompt(&mcp.Prompt{Name: "greet"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{}, nil
		})
	tc := newTestContext(t, child, upstream.Options{})

	tc.start(t)

	assert.True(t, tc.logs.contains("forwards tools only"),
		"expected a prominent warning that v1 does not forward the child's prompts/resources")
}

// TestUpstreamDefaultTransportRunsArtifact proves the default command
// transport through Start alone: the configured argv runs with {{artifact}}
// substituted in every element, the child inherits the proxy's environment
// (cmd.Env is set explicitly), and the child's stderr reaches the configured
// writer (the SDK's CommandTransport does not wire it). The artifact is a
// scratch script that prints its command line and one environment variable,
// then exits, so the MCP handshake fails — the expected error; the executed
// command line on stderr is the observable behavior. Shutdown escalation
// timing (TerminateDuration) needs a hung child and real signals; the E2E
// suite's TestE2EHungChildEscalation owns it.
//
// Not parallel: t.Setenv forbids t.Parallel.
func TestUpstreamDefaultTransportRunsArtifact(t *testing.T) {
	t.Setenv("MCP_DEVPROXY_TEST_CHILD_ENV", "inherited-ok")

	artifact := filepath.Join(t.TempDir(), "child-artifact")
	require.NoError(t,
		os.WriteFile(artifact,
			[]byte("#!/bin/sh\necho \"argv: $0 $*\" >&2\necho \"env: $MCP_DEVPROXY_TEST_CHILD_ENV\" >&2\n"),
			0o700),
		"write the child fixture script")

	var stderr bytes.Buffer
	up, err := upstream.New(upstream.Options{
		Argv:   []string{"{{artifact}}", "stdio", "--bin={{artifact}}"},
		Stderr: &stderr,
	})
	require.NoError(t, err, "construct upstream")

	_, err = up.Start(t.Context(), artifact)

	require.Error(t, err, "expected the handshake against a non-MCP child to fail")
	// Start reaps the child before returning (the transport's Close runs
	// cmd.Wait), so the buffer is complete and race-free to read here.
	assert.Contains(t, stderr.String(), "argv: "+artifact+" stdio --bin="+artifact,
		"expected {{artifact}} substituted in every argv element and the child's stderr wired to the configured writer")
	assert.Contains(t, stderr.String(), "env: inherited-ok",
		"expected the child to run with the proxy's environment, explicitly inherited")
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	_, err := upstream.New(upstream.Options{})
	require.Error(t, err, "expected New to require an argv template or a transport factory")

	_, err = upstream.New(upstream.Options{Argv: []string{"./child", "stdio"}})
	require.NoError(t, err, "expected an argv template alone to satisfy New")

	_, err = upstream.New(upstream.Options{Transport: func(string) (mcp.Transport, error) {
		transport, _ := mcp.NewInMemoryTransports()
		return transport, nil
	}})
	require.NoError(t, err, "expected a transport factory alone to satisfy New")

	_, err = upstream.New(upstream.Options{
		Argv:              []string{"./child", "stdio"},
		TerminateDuration: -time.Second,
	})
	require.ErrorContains(t, err, "terminate duration must not be negative",
		"expected New to reject a negative terminate duration instead of passing it to the transport")
}
