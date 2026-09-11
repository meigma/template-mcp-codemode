package cli

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
	hostmcp "github.com/meigma/codemode/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/meigma/template-mcp-codemode/internal/mcpserver"
)

// alicePolicy allows only the authenticated alice identity, irrespective of program metadata.
type alicePolicy struct{}

func (alicePolicy) Authorize(_ context.Context, input authz.AuthorizationInput) error {
	if input.Subject.ID != "alice" {
		return authz.ErrDenied
	}
	return nil
}

func TestHTTPAuthorizationUsesEachVerifiedIdentity(t *testing.T) {
	t.Parallel()

	server, err := mcpserver.New(mcpserver.Options{
		Version:  "test",
		Logger:   slog.New(slog.DiscardHandler),
		Resolver: hostmcp.ContextSubject(),
		Runtime:  codemode.Options{Authorizer: alicePolicy{}},
	})
	require.NoError(t, err)
	server.AddReceivingMiddleware(installHTTPSubject(true))
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token != "alice-credential" && token != "bob-credential" {
			return nil, auth.ErrInvalidToken
		}
		id := "bob"
		if token == "alice-credential" {
			id = "alice"
		}
		return &auth.TokenInfo{UserID: id, Expiration: time.Now().Add(time.Hour)}, nil
	}
	httpServer := httptest.NewServer(auth.RequireBearerToken(verifier, nil)(handler))
	t.Cleanup(httpServer.Close)
	alice := connectHTTPSession(t, httpServer.URL, "alice-credential")
	bob := connectHTTPSession(t, httpServer.URL, "bob-credential")

	for _, tc := range []struct {
		name    string
		session *mcp.ClientSession
		denied  bool
	}{
		{name: "bob cannot impersonate alice", session: bob, denied: true},
		{name: "alice remains authorized", session: alice},
		{name: "alice session does not authorize bob", session: bob, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, callErr := tc.session.CallTool(context.Background(), &mcp.CallToolParams{
				Meta:      mcp.Meta{"subject": map[string]any{"id": "alice"}, "subject_id": "alice"},
				Name:      "execute",
				Arguments: map[string]any{"source": "def main():\n    return random.int(min=7, max=7)"},
			})
			require.NoError(t, callErr)
			assert.Equal(t, tc.denied, result.IsError, "authorization must use the verified request identity")
		})
	}
}
