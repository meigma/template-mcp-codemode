package cli

import (
	"context"
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
)

// serverExitTimeout bounds how long tests wait for a serving function to
// return after its shutdown trigger fires.
const serverExitTimeout = 5 * time.Second

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

	// Prove the server is accepting requests before triggering shutdown. Any
	// HTTP response demonstrates liveness; a plain GET without an MCP session
	// is not a valid MCP request, so the status itself does not matter here.
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	require.NoError(t, err, "request to the running server")
	require.NoError(t, resp.Body.Close())

	cancel()

	select {
	case err := <-serveErr:
		require.NoError(t, err, "context cancellation is a clean shutdown")
	case <-time.After(serverExitTimeout):
		t.Fatal("serveHTTP did not return after context cancellation")
	}
}

// TestHTTPCommandReadsAddrFromEnvironment exercises the TEMPLATE_MCP_ADDR -> addr
// binding (the wiring most likely to break silently after the rename step). The
// fail-closed guard refuses the non-loopback address before any socket is bound,
// so the refusal error mentioning that address proves the env value reached the
// command.
func TestHTTPCommandReadsAddrFromEnvironment(t *testing.T) {
	t.Setenv("TEMPLATE_MCP_ADDR", "0.0.0.0:65535")

	root := NewRootCommand(Options{Viper: viper.New()})
	root.SetArgs([]string{httpCommandName})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.ExecuteContext(context.Background())

	require.ErrorContains(t, err, "0.0.0.0:65535", "the refusal must mention the env-provided address")
}

// TestEnvBindingResolvesHyphenatedFlag covers the SetEnvKeyReplacer hop that the
// addr test does not: the "auth-token" flag binds to TEMPLATE_MCP_AUTH_TOKEN
// (hyphen -> underscore). A regression dropping the replacer would break this
// while the hyphen-free addr key kept working, so it is tested explicitly. It
// binds flags directly rather than serving, keeping the test deterministic.
func TestEnvBindingResolvesHyphenatedFlag(t *testing.T) {
	t.Setenv("TEMPLATE_MCP_AUTH_TOKEN", "from-env")

	vp := viper.New()
	httpCmd := newHTTPCommand(Options{Viper: vp})
	require.NoError(t, initializeConfig(httpCmd, vp))

	assert.Equal(t, "from-env", vp.GetString(authTokenFlag))
}
