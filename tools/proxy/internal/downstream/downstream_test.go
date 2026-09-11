// Tests are whitebox: connecting an in-memory client requires the unexported
// server field.

package downstream

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// waitTimeout bounds every asynchronous wait in this suite.
const waitTimeout = 5 * time.Second

// tick is the polling interval for Never-style waits.
const tick = 10 * time.Millisecond

// quietWindow is how long a must-not-happen assertion watches; the SDK's
// notification coalescing delay is 10ms, so 300ms is comfortably past it.
const quietWindow = 300 * time.Millisecond

// forwardedText marks results produced by the recording CallToolFunc.
const forwardedText = "forwarded-by-recording-call"

// testContext bundles the frontend under test, an in-memory client session
// standing in for Claude, and the channels its handlers feed.
type testContext struct {
	frontend    *Frontend
	session     *mcp.ClientSession
	calls       chan *mcp.CallToolParams
	listChanged chan struct{}
	logs        chan *mcp.LoggingMessageParams
}

func newTestContext(t *testing.T) *testContext {
	t.Helper()

	frontend, err := New(Options{})
	require.NoError(t, err, "construct frontend")

	tc := &testContext{
		frontend:    frontend,
		calls:       make(chan *mcp.CallToolParams, 16),
		listChanged: make(chan struct{}, 16),
		logs:        make(chan *mcp.LoggingMessageParams, 16),
	}
	tc.session = tc.connectClient(t)
	return tc
}

// connectClient connects one real in-memory mcp.Client to the frontend's
// server (servers must be connected before clients) with handlers feeding
// the shared channels.
func (tc *testContext) connectClient(t *testing.T) *mcp.ClientSession {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	_, err := tc.frontend.server.Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err, "connect the frontend's server side")

	client := mcp.NewClient(
		&mcp.Implementation{Name: "fake-claude", Version: "0.0.1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				select {
				case tc.listChanged <- struct{}{}:
				default:
				}
			},
			LoggingMessageHandler: func(_ context.Context, req *mcp.LoggingMessageRequest) {
				select {
				case tc.logs <- req.Params:
				default:
				}
			},
		},
	)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err, "connect the in-memory client")
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// reconcile drives Frontend.Reconcile with the recording CallToolFunc.
func (tc *testContext) reconcile(t *testing.T, tools ...*mcp.Tool) {
	t.Helper()

	require.NoError(t, tc.frontend.Reconcile(tools, tc.recordingCall()), "reconcile tool set")
}

// recordingCall returns a CallToolFunc that records forwarded params and
// answers with a canned success result.
func (tc *testContext) recordingCall() reloader.CallToolFunc {
	return func(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		tc.calls <- params
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: forwardedText}},
		}, nil
	}
}

// awaitListChanged waits for one tools/list_changed to reach the client.
func (tc *testContext) awaitListChanged(t *testing.T) {
	t.Helper()

	select {
	case <-tc.listChanged:
	case <-time.After(waitTimeout):
		t.Fatal("expected a tools/list_changed notification")
	}
}

// requireNoListChanged asserts no notification arrives within quietWindow —
// the observable proof of zero AddTool/RemoveTools calls, since the SDK emits
// a (coalesced) tools/list_changed for every mutation.
func (tc *testContext) requireNoListChanged(t *testing.T) {
	t.Helper()

	assert.Never(t, func() bool {
		select {
		case <-tc.listChanged:
			return true
		default:
			return false
		}
	}, quietWindow, tick, "expected no tools/list_changed (zero AddTool/RemoveTools calls)")
}

// requireStale asserts result is the friendly stale-reload tool result.
func (tc *testContext) requireStale(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()

	require.True(t, result.IsError, "expected the friendly stale-reload error result")
	assert.Contains(t, resultText(t, result), "changed by dev reload",
		"expected the stale-reload message the LLM can read")
}

// requireForwarded asserts result came from the recording CallToolFunc and
// returns the params it captured.
func (tc *testContext) requireForwarded(t *testing.T, result *mcp.CallToolResult) *mcp.CallToolParams {
	t.Helper()

	require.False(t, result.IsError, "expected the call to dispatch, not an error result: %s",
		resultText(t, result))
	assert.Equal(t, forwardedText, resultText(t, result),
		"expected the recording CallToolFunc's canned result")
	select {
	case params := <-tc.calls:
		return params
	case <-time.After(waitTimeout):
		t.Fatal("expected the call to be forwarded through CallToolFunc")
		return nil
	}
}

// callTool issues one tools/call from session with empty arguments.
func callTool(t *testing.T, session *mcp.ClientSession, name string) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(
		t.Context(),
		&mcp.CallToolParams{Name: name, Arguments: map[string]any{}},
	)
	require.NoError(t, err, "expected a tool result for %q, not a protocol error", name)
	return result
}

// listNames lists tools from session and returns their names; the list also
// records the session's observed generation for the stale-view gate.
func listNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err, "list tools")
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// resultText extracts the first text content of a tool result.
func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) == 0 {
		return ""
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content in the tool result")
	return text.Text
}

// namedTool returns a minimal valid tool definition. Each call returns a
// fresh value, so two calls build deep-copied identical definitions.
func namedTool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
}

// schemaTool returns a tool whose input schema declares one string property,
// distinguishing its fingerprint from namedTool's bare object schema.
func schemaTool(name, property string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{property: map[string]any{"type": "string"}},
	}}
}

// readOnlyTool is namedTool plus a readOnlyHint annotation — the
// fingerprint-sensitivity case where only annotations differ.
func readOnlyTool(name string) *mcp.Tool {
	tool := namedTool(name)
	tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
	return tool
}

func TestFrontendCapabilityEnvelope(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)

	result := tc.session.InitializeResult()
	require.NotNil(t, result, "expected the initialize handshake to have completed")
	caps := result.Capabilities
	require.NotNil(t, caps, "expected server capabilities in the initialize result")
	require.NotNil(t, caps.Tools, "expected the tools capability even with zero tools advertised")
	assert.True(t, caps.Tools.ListChanged, "expected tools list_changed in the superset envelope")
	require.NotNil(t, caps.Prompts, "expected the prompts capability in the superset envelope")
	assert.True(t, caps.Prompts.ListChanged, "expected prompts list_changed in the superset envelope")
	require.NotNil(t, caps.Resources, "expected the resources capability in the superset envelope")
	assert.True(t, caps.Resources.ListChanged, "expected resources list_changed in the superset envelope")
	assert.NotNil(t, caps.Logging, "expected the logging capability in the superset envelope")

	assert.Empty(t, listNames(t, tc.session),
		"expected an instantly served, empty tool list before the first reconcile")
}

func TestFrontendRawArgumentPassthrough(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.reconcile(t, namedTool("alpha"))
	tc.awaitListChanged(t)
	require.Equal(t, []string{"alpha"}, listNames(t, tc.session),
		"expected the reconciled tool to be advertised")

	raw := json.RawMessage(`{"z":1,"a":{"deep":[1,2,3]},"unknown_field":"kept","s":"text"}`)
	result, err := tc.session.CallTool(t.Context(), &mcp.CallToolParams{
		Meta:      mcp.Meta{"example.com/trace": "drop-me"},
		Name:      "alpha",
		Arguments: raw,
	})
	require.NoError(t, err, "call the advertised tool")

	params := tc.requireForwarded(t, result)
	forwardedRaw, ok := params.Arguments.(json.RawMessage)
	require.True(t, ok, "expected the wire json.RawMessage to be forwarded, not a decoded value")
	assert.Equal(t, string(raw), string(forwardedRaw),
		"expected byte-for-byte argument passthrough with no validation or defaulting")
	assert.Nil(t, params.Meta, "expected Meta (and any progress token) to be dropped")
}

func TestFrontendReconcileTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		initial    []*mcp.Tool
		next       []*mcp.Tool
		wantNames  []string
		assertTool func(t *testing.T, tools []*mcp.Tool)
	}{
		{
			name:      "adds a new tool",
			initial:   []*mcp.Tool{namedTool("alpha")},
			next:      []*mcp.Tool{namedTool("alpha"), namedTool("beta")},
			wantNames: []string{"alpha", "beta"},
		},
		{
			name:      "removes a tool",
			initial:   []*mcp.Tool{namedTool("alpha"), namedTool("beta")},
			next:      []*mcp.Tool{namedTool("alpha")},
			wantNames: []string{"alpha"},
		},
		{
			name:      "replaces a changed schema in place",
			initial:   []*mcp.Tool{namedTool("alpha")},
			next:      []*mcp.Tool{schemaTool("alpha", "city")},
			wantNames: []string{"alpha"},
			assertTool: func(t *testing.T, tools []*mcp.Tool) {
				t.Helper()
				schema, ok := tools[0].InputSchema.(map[string]any)
				require.True(t, ok, "expected the listed schema as a map")
				assert.Contains(t, schema, "properties",
					"expected the replaced definition's new schema to be listed")
			},
		},
		{
			name:      "annotations-only change counts as changed",
			initial:   []*mcp.Tool{namedTool("alpha")},
			next:      []*mcp.Tool{readOnlyTool("alpha")},
			wantNames: []string{"alpha"},
			assertTool: func(t *testing.T, tools []*mcp.Tool) {
				t.Helper()
				require.NotNil(t, tools[0].Annotations, "expected the new annotations to be listed")
				assert.True(t, tools[0].Annotations.ReadOnlyHint,
					"expected the readOnlyHint flip to reach the client")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := newTestContext(t)
			tc.reconcile(t, tt.initial...)
			tc.awaitListChanged(t)

			tc.reconcile(t, tt.next...)
			tc.awaitListChanged(t)

			result, err := tc.session.ListTools(t.Context(), nil)
			require.NoError(t, err, "list tools after the transition")
			names := make([]string, 0, len(result.Tools))
			for _, tool := range result.Tools {
				names = append(names, tool.Name)
			}
			assert.ElementsMatch(t, tt.wantNames, names,
				"expected the advertised set to match the reconciled one")
			if tt.assertTool != nil {
				tt.assertTool(t, result.Tools)
			}
		})
	}
}

func TestFrontendNoOpDiffMakesNoCalls(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.reconcile(t, namedTool("alpha"), readOnlyTool("beta"))
	tc.awaitListChanged(t)
	listNames(t, tc.session)

	// A deep-copied identical set: fresh builder values, same wire form.
	tc.reconcile(t, namedTool("alpha"), readOnlyTool("beta"))

	tc.requireNoListChanged(t)
	// The call dispatching without a fresh tools/list proves the no-op diff
	// also left the stale-view clock untouched.
	tc.requireForwarded(t, callTool(t, tc.session, "alpha"))
}

func TestFrontendStaleViewGating(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.reconcile(t, schemaTool("alpha", "old"))
	listNames(t, tc.session)

	tc.reconcile(t, schemaTool("alpha", "new"))

	// A call to the changed tool from a session that has not re-listed gets
	// the friendly error and never reaches the child.
	result := callTool(t, tc.session, "alpha")
	tc.requireStale(t, result)
	assert.Empty(t, tc.calls, "expected the gated call never to reach CallToolFunc")

	// After the session re-lists, the same call dispatches.
	listNames(t, tc.session)
	result = callTool(t, tc.session, "alpha")
	tc.requireForwarded(t, result)
}

func TestFrontendNeverListedSessionGatesStale(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.reconcile(t, namedTool("alpha"))
	tc.awaitListChanged(t)

	// A session that connects after the mutating Reconcile and never lists
	// holds an unknown view: unknown never matches, so its calls gate stale
	// instead of dispatching blind.
	sessionB := tc.connectClient(t)
	tc.requireStale(t, callTool(t, sessionB, "alpha"))
	assert.Empty(t, tc.calls, "expected the never-listed session's call not to reach CallToolFunc")

	// The session's first successful tools/list opens the gate.
	listNames(t, sessionB)
	tc.requireForwarded(t, callTool(t, sessionB, "alpha"))
}

func TestFrontendTombstoneAndReAdd(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.reconcile(t, namedTool("omega"))
	listNames(t, tc.session)

	// Removal: the tombstone upgrades the SDK's raw "unknown tool" protocol
	// error to the friendly stale result (callTool fails on protocol errors).
	tc.reconcile(t)
	result := callTool(t, tc.session, "omega")
	tc.requireStale(t, result)

	// Re-adding the name clears the tombstone; after a re-list, calls flow.
	tc.reconcile(t, namedTool("omega"))
	listNames(t, tc.session)
	result = callTool(t, tc.session, "omega")
	tc.requireForwarded(t, result)
}

func TestFrontendPerSessionIsolation(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	sessionB := tc.connectClient(t)

	tc.reconcile(t, schemaTool("gamma", "old"))
	listNames(t, tc.session)
	listNames(t, sessionB)

	tc.reconcile(t, schemaTool("gamma", "new"))

	// Session B re-lists and calls fine; stale session A stays gated.
	listNames(t, sessionB)
	tc.requireForwarded(t, callTool(t, sessionB, "gamma"))
	tc.requireStale(t, callTool(t, tc.session, "gamma"))
}

func TestFrontendLoggingWiring(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	assert.Empty(t, string(tc.frontend.Level()), "expected no level before the client sets one")

	require.NoError(t,
		tc.session.SetLoggingLevel(t.Context(), &mcp.SetLoggingLevelParams{Level: "warning"}),
		"set the logging level from the client")
	assert.Equal(t, mcp.LoggingLevel("warning"), tc.frontend.Level(),
		"expected the middleware to record the client's level for the upstream adapter")

	tc.frontend.Log(t.Context(), &mcp.LoggingMessageParams{Level: "error", Data: "loud"})
	select {
	case params := <-tc.logs:
		assert.Equal(t, mcp.LoggingLevel("error"), params.Level,
			"expected the message at or above the client's level to be forwarded")
	case <-time.After(waitTimeout):
		t.Fatal("expected the error-level message to reach the client's logging handler")
	}

	tc.frontend.Log(t.Context(), &mcp.LoggingMessageParams{Level: "debug", Data: "quiet"})
	assert.Never(t, func() bool {
		select {
		case <-tc.logs:
			return true
		default:
			return false
		}
	}, quietWindow, tick, "expected the below-level message to be dropped by the SDK's session gate")
}

func TestFrontendReconcileValidationError(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)

	err := tc.frontend.Reconcile([]*mcp.Tool{{Name: "broken"}}, tc.recordingCall())

	require.Error(t, err, "expected an error, not a panic, for a definition without an input schema")
	require.ErrorContains(t, err, "missing input schema", "expected the validation failure to say why")
	assert.Empty(t, listNames(t, tc.session), "expected nothing to be advertised after the failure")
	tc.requireNoListChanged(t)
}
