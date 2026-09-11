package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServerEndToEnd connects an MCP client to the server over the SDK's
// in-memory transport pair and exercises the full tools/list and tools/call
// path, asserting the random_int tool is discoverable and returns structured
// output within the requested range.
func TestServerEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session := newClientSession(t)

	// tools/list: the server must expose exactly its expected tool set, so a
	// forgotten registration (or an accidental extra tool) fails here.
	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err, "list tools")

	names := make([]string, 0, len(tools.Tools))
	byName := make(map[string]*mcp.Tool, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	assert.ElementsMatch(t, []string{"random_int"}, names,
		"tools/list must expose exactly the expected tool set")

	randomIntTool := byName["random_int"]
	require.NotNil(t, randomIntTool, "tools/list must include random_int")
	require.NotNil(t, randomIntTool.InputSchema, "random_int must publish a JSON Schema object")

	// tools/call: the structured Value must fall within the requested range.
	const wantMin, wantMax = 3, 7
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "random_int",
		Arguments: map[string]any{"min": wantMin, "max": wantMax},
	})
	require.NoError(t, err, "call tool")
	require.False(t, result.IsError, "tool call failed, content: %+v", result.Content)

	// StructuredContent round-trips through JSON; decode it into the typed shape.
	var out randomIntOutput
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err, "marshal structured content")
	require.NoError(t, json.Unmarshal(raw, &out), "unmarshal structured content %q", raw)
	assert.GreaterOrEqual(t, out.Value, wantMin)
	assert.LessOrEqual(t, out.Value, wantMax)
}

// TestServerEndToEndToolError verifies the tool-error convention: an invalid
// range yields a CallToolResult with IsError set, not a protocol error.
func TestServerEndToEndToolError(t *testing.T) {
	t.Parallel()

	session := newClientSession(t)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "random_int",
		Arguments: map[string]any{"min": 10, "max": 1},
	})

	require.NoError(t, err, "min > max must be a tool-level error, not a protocol error")
	assert.True(t, result.IsError, "call tool result must set IsError for min > max")
}

// newClientSession connects an MCP client to a freshly constructed template
// server over the SDK's in-memory transport pair and returns the client side,
// closing both sessions when the test finishes.
func newClientSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	srv := New(Options{Version: "test", Logger: slog.New(slog.DiscardHandler)})
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	require.NoError(t, err, "server connect")
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err, "client connect")
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}
