package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
	hostmcp "github.com/meigma/codemode/mcpserver"
)

const trustedSubjectID authz.SubjectID = "local"

type executeEnvelope struct {
	Result randomIntOutput `json:"result"`
}

// localRangePolicy authorizes the local subject only for ranges ending above one.
type localRangePolicy struct{}

func (localRangePolicy) Authorize(_ context.Context, input authz.AuthorizationInput) error {
	maximum, valid := input.Arguments["max"].(int64)
	if input.Subject.ID != trustedSubjectID || !valid || maximum <= 1 {
		return authz.ErrDenied
	}
	return nil
}

func TestServerEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session := newClientSession(t, Options{})

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err, "list tools")

	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"search_api", "describe_api", "execute"}, names,
		"tools/list must expose exactly the CodeMode tool set")

	searched, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_api",
		Arguments: map[string]any{"query": randomIntName},
	})
	require.NoError(t, err, "search_api")
	requireSuccessfulTool(t, searched)
	var search codemode.SearchResponse
	decodeStructured(t, searched, &search)
	require.NotEmpty(t, search.Results, "search_api must find random.int")
	assert.Equal(t, randomIntName, search.Results[0].Name)

	described, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "describe_api",
		Arguments: map[string]any{"name": randomIntName},
	})
	require.NoError(t, err, "describe_api")
	requireSuccessfulTool(t, described)
	var description codemode.Description
	decodeStructured(t, described, &description)
	assert.Equal(t, randomIntName, description.Name)
	require.Len(t, description.Input, 2)
	assert.Equal(t, "min", description.Input[0].Name)
	assert.Equal(t, "int", description.Input[0].Type)
	assert.True(t, description.Input[0].Required)
	assert.Equal(t, "max", description.Input[1].Name)
	assert.Equal(t, "int", description.Input[1].Type)
	assert.True(t, description.Input[1].Required)

	const wantMin, wantMax int64 = 3, 7
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": randomIntProgram(wantMin, wantMax)},
	})
	require.NoError(t, err, "execute")
	requireSuccessfulTool(t, result)

	var out executeEnvelope
	decodeStructured(t, result, &out)
	assert.GreaterOrEqual(t, out.Result.Value, wantMin)
	assert.LessOrEqual(t, out.Result.Value, wantMax)
}

func TestServerEndToEndEqualBounds(t *testing.T) {
	t.Parallel()

	session := newClientSession(t, Options{})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": randomIntProgram(5, 5)},
	})
	require.NoError(t, err, "execute")
	requireSuccessfulTool(t, result)

	var out executeEnvelope
	decodeStructured(t, result, &out)
	assert.Equal(t, int64(5), out.Result.Value)
}

func TestServerEndToEndToolError(t *testing.T) {
	t.Parallel()

	session := newClientSession(t, Options{})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": randomIntProgram(10, 1)},
	})

	require.NoError(t, err, "min > max must be a tool-level error, not a protocol error")
	requireToolError(t, result, codemode.ErrCapabilityFailure.Error())
}

func TestServerRejectsMissingSubject(t *testing.T) {
	t.Parallel()

	session := newClientSession(t, Options{Resolver: hostmcp.ContextSubject()})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": randomIntProgram(1, 1)},
	})
	require.NoError(t, err, "missing subject must be a tool-level error")
	requireToolError(t, result, codemode.ErrUnauthenticated.Error())
}

func TestServerAuthorizesTrustedSubjectAndArguments(t *testing.T) {
	t.Parallel()

	session := newClientSession(t, Options{
		Runtime: codemode.Options{Authorizer: localRangePolicy{}},
	})

	allowed, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Meta: mcp.Meta{
			"subject_id": "subject-attacker",
			"subject":    map[string]any{"id": "subject-attacker"},
		},
		Name:      "execute",
		Arguments: map[string]any{"source": randomIntProgram(5, 5)},
	})
	require.NoError(t, err, "execute")
	requireSuccessfulTool(t, allowed)

	denied, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Meta: mcp.Meta{
			"subject_id": "subject-attacker",
		},
		Name:      "execute",
		Arguments: map[string]any{"source": randomIntProgram(0, 1)},
	})
	require.NoError(t, err, "denied execute must stay a tool-level error")
	requireToolError(t, denied, codemode.ErrPermissionDenied.Error())
}

func TestServerRequiresAuthorizer(t *testing.T) {
	t.Parallel()

	_, err := New(Options{
		Version:  "test",
		Logger:   slog.New(slog.DiscardHandler),
		Resolver: hostmcp.StaticSubject(authz.Subject{ID: trustedSubjectID}),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, codemode.ErrInvalidRegistration)
}

func TestServerRequiresResolver(t *testing.T) {
	t.Parallel()

	_, err := New(Options{
		Version: "test",
		Logger:  slog.New(slog.DiscardHandler),
		Runtime: codemode.Options{Authorizer: authz.AllowAll()},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, codemode.ErrInvalidRegistration)
}

func newClientSession(t *testing.T, options Options) *mcp.ClientSession {
	t.Helper()

	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	if options.Version == "" {
		options.Version = "test"
	}
	if options.Resolver == nil {
		options.Resolver = hostmcp.StaticSubject(authz.Subject{ID: trustedSubjectID})
	}
	if options.Runtime.Authorizer == nil {
		options.Runtime.Authorizer = authz.AllowAll()
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	srv, err := New(options)
	require.NoError(t, err, "construct server")
	serverSession, err := srv.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err, "server connect")
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err, "client connect")
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func randomIntProgram(minimum, maximum int64) string {
	return fmt.Sprintf("def main():\n    return random.int(min=%d, max=%d)\n", minimum, maximum)
}

func decodeStructured(t *testing.T, result *mcp.CallToolResult, dest any) {
	t.Helper()

	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err, "marshal structured content")
	require.NoError(t, json.Unmarshal(raw, dest), "unmarshal structured content %q", raw)
}

func requireSuccessfulTool(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, "tool call failed, content: %+v", result.Content)
}

func requireToolError(t *testing.T, result *mcp.CallToolResult, expected string) {
	t.Helper()
	require.NotNil(t, result)
	require.True(t, result.IsError, "expected a tool-level error")
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "tool error content must be text")
	assert.Equal(t, expected, text.Text)
}
