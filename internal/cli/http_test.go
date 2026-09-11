package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/template-mcp-codemode/internal/templateinfo"
)

// serverExitTimeout bounds how long tests wait for a serving function to
// return after its shutdown trigger fires.
const serverExitTimeout = 5 * time.Second

// httpReadyTimeout bounds how long tests wait for serveHTTP to finish the
// CodeMode worker probe and start accepting requests.
const httpReadyTimeout = 20 * time.Second

func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want bool
	}{
		{addr: "localhost:8080", want: true},
		{addr: "127.0.0.1:8080", want: true},
		{addr: "[::1]:8080", want: true},
		{addr: "localhost", want: true},
		{addr: "127.0.0.1", want: true},
		{addr: "0.0.0.0:8080", want: false},
		{addr: ":8080", want: false},
		{addr: "192.168.1.10:8080", want: false},
		{addr: "example.com:8080", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, isLoopbackHost(tt.addr))
		})
	}
}

func TestCheckBindSecurity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		addr      string
		authToken string
		insecure  bool
		wantErr   bool
	}{
		{name: "loopback without auth is allowed", addr: "localhost:8080"},
		{name: "loopback ip without auth is allowed", addr: "127.0.0.1:8080"},
		{name: "non-loopback with auth is allowed", addr: "0.0.0.0:8080", authToken: "secret"},
		{name: "non-loopback with insecure is allowed", addr: "0.0.0.0:8080", insecure: true},
		{name: "non-loopback without auth is refused", addr: "0.0.0.0:8080", wantErr: true},
		{name: "all interfaces without auth is refused", addr: ":8080", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkBindSecurity(tt.addr, tt.authToken, tt.insecure)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRequireBearerToken(t *testing.T) {
	t.Parallel()

	const token = "s3cret-token"
	middleware := requireBearerToken(token, "localhost:8080")

	tests := []struct {
		name          string
		authHeader    string
		wantStatus    int
		wantReached   bool
		wantChallenge bool
	}{
		{name: "missing token is rejected", authHeader: "", wantStatus: http.StatusUnauthorized, wantChallenge: true},
		{
			name:          "wrong token is rejected",
			authHeader:    "Bearer wrong",
			wantStatus:    http.StatusUnauthorized,
			wantChallenge: true,
		},
		{name: "correct token passes", authHeader: "Bearer " + token, wantStatus: http.StatusOK, wantReached: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reached bool
			handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantReached, reached, "whether the wrapped handler is reached")
			if tt.wantChallenge {
				assert.NotEmpty(
					t,
					rec.Header().Get("WWW-Authenticate"),
					"rejections must carry a WWW-Authenticate challenge",
				)
			}
		})
	}
}

// TestServeHTTPShutsDownOnContextCancel proves the graceful-shutdown path: a
// running server stops accepting connections and serveHTTP returns nil (not an
// error) when the context is cancelled, the same way a SIGINT/SIGTERM-derived
// context behaves in production.
func TestServeHTTPShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen on an ephemeral port")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveHTTP(ctx, ln, httpConfig{
			build:  BuildInfo{Version: "test"},
			addr:   ln.Addr().String(),
			logger: slog.New(slog.DiscardHandler),
		})
	}()

	waitForHTTP(t, "http://"+ln.Addr().String())

	cancel()

	select {
	case err := <-serveErr:
		require.NoError(t, err, "context cancellation is a clean shutdown")
	case <-time.After(serverExitTimeout):
		t.Fatal("serveHTTP did not return after context cancellation")
	}
}

// TestHTTPCommandReadsAddrFromEnvironment exercises the
// TEMPLATE_MCP_CODEMODE_ADDR -> addr binding (the wiring most likely to break
// silently after the rename step). The fail-closed guard refuses the
// non-loopback address before any socket is bound, so the refusal error
// mentioning that address proves the env value reached the command.
func TestHTTPCommandReadsAddrFromEnvironment(t *testing.T) {
	t.Setenv(templateinfo.EnvPrefix()+"_ADDR", "0.0.0.0:65535")

	root := NewRootCommand(Options{Viper: viper.New()})
	root.SetArgs([]string{httpCommandName})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.ExecuteContext(context.Background())

	require.ErrorContains(t, err, "0.0.0.0:65535", "the refusal must mention the env-provided address")
}

// TestEnvBindingResolvesHyphenatedFlag covers the SetEnvKeyReplacer hop that the
// addr test does not: the "auth-token" flag binds to TEMPLATE_MCP_CODEMODE_AUTH_TOKEN
// (hyphen -> underscore). A regression dropping the replacer would break this
// while the hyphen-free addr key kept working, so it is tested explicitly. It
// binds flags directly rather than serving, keeping the test deterministic.
func TestEnvBindingResolvesHyphenatedFlag(t *testing.T) {
	t.Setenv(templateinfo.EnvPrefix()+"_AUTH_TOKEN", "from-env")

	vp := viper.New()
	httpCmd := newHTTPCommand(Options{Viper: vp})
	require.NoError(t, initializeConfig(httpCmd, vp))

	assert.Equal(t, "from-env", vp.GetString(authTokenFlag))
}

func TestServeHTTPExposesCodeModeTools(t *testing.T) {
	t.Parallel()

	session, stop := startHTTPSession(t, httpConfig{
		build:  BuildInfo{Version: "test"},
		logger: slog.New(slog.DiscardHandler),
	}, nil)
	defer stop()

	assertCodeModeExecute(t, session)
}

func TestServeHTTPRejectsMissingBearerThenServes(t *testing.T) {
	t.Parallel()

	const token = "s3cret-token"
	ln := startHTTPListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveHTTP(ctx, ln, httpConfig{
			build:     BuildInfo{Version: "test"},
			addr:      ln.Addr().String(),
			authToken: token,
			logger:    slog.New(slog.DiscardHandler),
		})
	}()

	endpoint := "http://" + ln.Addr().String()
	waitForHTTP(t, endpoint)

	resp, err := http.Get(endpoint + "/")
	require.NoError(t, err, "unauthenticated request")
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	session := connectHTTPSession(t, endpoint, token)
	assertCodeModeExecute(t, session)

	cancel()
	select {
	case err := <-serveErr:
		require.NoError(t, err, "context cancellation is a clean shutdown")
	case <-time.After(serverExitTimeout):
		t.Fatal("serveHTTP did not return after context cancellation")
	}
}

func startHTTPListener(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen on an ephemeral port")
	return ln
}

func startHTTPSession(t *testing.T, cfg httpConfig, token *string) (*mcp.ClientSession, context.CancelFunc) {
	t.Helper()

	ln := startHTTPListener(t)
	cfg.addr = ln.Addr().String()
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveHTTP(ctx, ln, cfg)
	}()

	endpoint := "http://" + ln.Addr().String()
	waitForHTTP(t, endpoint)

	authToken := ""
	if token != nil {
		authToken = *token
	}
	session := connectHTTPSession(t, endpoint, authToken)
	return session, func() {
		_ = session.Close()
		cancel()
		select {
		case err := <-serveErr:
			require.NoError(t, err, "HTTP server shutdown")
		case <-time.After(serverExitTimeout):
			t.Fatal("serveHTTP did not return after context cancellation")
		}
	}
}

func connectHTTPSession(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()

	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
	}
	if token != "" {
		transport.HTTPClient = &http.Client{Transport: bearerRoundTripper{token: token}}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	require.NoError(t, err, "HTTP MCP connect")
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func waitForHTTP(t *testing.T, endpoint string) {
	t.Helper()

	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(httpReadyTimeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(endpoint + "/")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("HTTP server did not become reachable")
}

func assertCodeModeExecute(t *testing.T, session *mcp.ClientSession) {
	t.Helper()

	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err, "tools/list over HTTP")
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"search_api", "describe_api", "execute"}, names)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": "def main():\n    return random.int(min=5, max=5)\n"},
	})
	require.NoError(t, err, "execute over HTTP")
	require.False(t, result.IsError, "HTTP execute failed, content: %+v", result.Content)

	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var envelope struct {
		Result struct {
			Value int64 `json:"value"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.Equal(t, int64(5), envelope.Result.Value)
}

type bearerRoundTripper struct {
	token string
}

func (transport bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+transport.token)
	return http.DefaultTransport.RoundTrip(req)
}
