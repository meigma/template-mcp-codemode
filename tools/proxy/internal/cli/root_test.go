// Tests are whitebox: the resolved config and the launch seam are
// unexported, and the repo intentionally uses in-package tests.

package cli

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// executeResolve runs the real root command — flag parsing, viper binding,
// environment, zero-config defaults, validation — with a recording
// launchFunc, returning the resolved config, the command's stderr, and the
// execution error. The proxy itself is never wired or started.
func executeResolve(t *testing.T, args []string) (config, string, error) {
	t.Helper()

	var got config
	launched := false
	var stderr bytes.Buffer
	root := newRootCommand(
		Options{Err: &stderr},
		func(_ *cobra.Command, cfg config, _ *slog.Logger) error {
			got = cfg
			launched = true
			return nil
		},
	)
	root.SetArgs(args)

	err := root.ExecuteContext(t.Context())
	if err == nil {
		require.True(t, launched, "expected a successful execution to reach the launch seam")
	} else {
		require.False(t, launched, "expected a failed execution never to reach the launch seam")
	}
	return got, stderr.String(), err
}

// discardLogger returns a logger for paths whose output is not under test.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// validProxyConfig returns a config newProxy accepts without running
// anything: construction touches no watch paths and spawns no processes.
func validProxyConfig() config {
	return config{
		buildCommand: "go build -o {{artifact}} ./cmd/template-mcp",
		watchDirs:    []string{"."},
		childArgv:    []string{"{{artifact}}", "stdio"},
	}
}

// Not parallel: the environment cases use t.Setenv.
func TestRootCommandConfigResolution(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want config
	}{
		{
			name: "flags resolve into config",
			args: []string{
				"--build", "make {{artifact}}",
				"--dir", "subdir",
				"--watch", "a", "--watch", "b c",
				"--debounce", "10ms", "--quiesce", "20ms", "--terminate", "30ms",
				"--", "{{artifact}}", "stdio", "--child-flag",
			},
			want: config{
				buildCommand: "make {{artifact}}",
				buildDir:     "subdir",
				watchDirs:    []string{"a", "b c"},
				childArgv:    []string{"{{artifact}}", "stdio", "--child-flag"},
				debounce:     10 * time.Millisecond,
				quiesce:      20 * time.Millisecond,
				terminate:    30 * time.Millisecond,
			},
		},
		{
			name: "environment variables resolve when flags are absent",
			env: map[string]string{
				"MCP_DEVPROXY_BUILD":    "make {{artifact}}",
				"MCP_DEVPROXY_DIR":      "envdir",
				"MCP_DEVPROXY_WATCH":    "x y",
				"MCP_DEVPROXY_DEBOUNCE": "40ms",
			},
			args: []string{"--", "{{artifact}}", "stdio"},
			want: config{
				buildCommand: "make {{artifact}}",
				buildDir:     "envdir",
				watchDirs:    []string{"x", "y"},
				childArgv:    []string{"{{artifact}}", "stdio"},
				debounce:     40 * time.Millisecond,
				quiesce:      defaultQuiesce,
				terminate:    defaultTerminate,
			},
		},
		{
			name: "flags beat environment variables",
			env: map[string]string{
				"MCP_DEVPROXY_BUILD":    "env {{artifact}}",
				"MCP_DEVPROXY_DEBOUNCE": "1s",
			},
			args: []string{
				"--build", "flag {{artifact}}",
				"--debounce", "10ms",
				"--watch", "w",
				"--", "{{artifact}}", "stdio",
			},
			want: config{
				buildCommand: "flag {{artifact}}",
				watchDirs:    []string{"w"},
				childArgv:    []string{"{{artifact}}", "stdio"},
				debounce:     10 * time.Millisecond,
				quiesce:      defaultQuiesce,
				terminate:    defaultTerminate,
			},
		},
		{
			name: "zero config selects the template defaults",
			args: nil,
			want: config{
				buildCommand: defaultBuildCommand,
				watchDirs:    defaultWatchDirs(),
				childArgv:    defaultChildArgv(),
				debounce:     defaultDebounce,
				quiesce:      defaultQuiesce,
				terminate:    defaultTerminate,
			},
		},
		{
			name: "partial flags keep the remaining template defaults",
			args: []string{"--build", "custom {{artifact}}"},
			want: config{
				buildCommand: "custom {{artifact}}",
				watchDirs:    defaultWatchDirs(),
				childArgv:    defaultChildArgv(),
				debounce:     defaultDebounce,
				quiesce:      defaultQuiesce,
				terminate:    defaultTerminate,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			got, _, err := executeResolve(t, tt.args)

			require.NoError(t, err, "resolve the configuration")
			assert.Equal(t, tt.want, got, "expected the resolved config to match")
		})
	}
}

func TestRootCommandZeroConfigLogsDefaults(t *testing.T) {
	t.Parallel()

	_, stderr, err := executeResolve(t, nil)

	require.NoError(t, err, "resolve the zero-config invocation")
	for _, flag := range []string{buildFlag, watchFlag} {
		assert.Contains(t, stderr, "--"+flag,
			"expected the defaulted %s value to be logged so zero-config is never silent", flag)
	}
	assert.Contains(t, stderr, "template default",
		"expected the defaults layer to announce itself on stderr")
}

func TestRootCommandValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "positional args without the dash separator",
			args:    []string{"./server", "stdio"},
			wantErr: `must follow "--"`,
		},
		{
			name:    "stray args before the dash separator",
			args:    []string{"stray", "--", "{{artifact}}", "stdio"},
			wantErr: `no arguments are allowed before "--"`,
		},
		{
			name:    "dash separator with no child command",
			args:    []string{"--"},
			wantErr: `child command is required after "--"`,
		},
		{
			name:    "dash separator with no child command alongside flags",
			args:    []string{"--build", "make {{artifact}}", "--watch", ".", "--"},
			wantErr: `child command is required after "--"`,
		},
		{
			name:    "child argv without the artifact token",
			args:    []string{"--", "./server", "stdio"},
			wantErr: artifactToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := executeResolve(t, tt.args)

			require.Error(t, err, "expected the configuration to be rejected")
			assert.ErrorContains(t, err, tt.wantErr, "expected the rejection to say why")
		})
	}
}

// TestValidateConfigGenericLayer exercises the validation branches the
// zero-config defaults layer makes unreachable through the command: they are
// the generic contract that must survive extraction to a standalone repo,
// where defaults.go is deleted.
func TestValidateConfigGenericLayer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(cfg *config)
		wantErr string
	}{
		{
			name:    "empty build command",
			mutate:  func(cfg *config) { cfg.buildCommand = " " },
			wantErr: "--" + buildFlag,
		},
		{
			name:    "no watch directories",
			mutate:  func(cfg *config) { cfg.watchDirs = nil },
			wantErr: "--" + watchFlag,
		},
		{
			name:    "no child command",
			mutate:  func(cfg *config) { cfg.childArgv = nil },
			wantErr: "child command is required",
		},
		{
			name:    "child command without the artifact token",
			mutate:  func(cfg *config) { cfg.childArgv = []string{"./server", "stdio"} },
			wantErr: artifactToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validProxyConfig()
			tt.mutate(&cfg)

			err := validateConfig(cfg)

			require.Error(t, err, "expected the configuration to be rejected")
			assert.ErrorContains(t, err, tt.wantErr, "expected the rejection to name the cause")
		})
	}
}

func TestNewProxyConstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(cfg *config)
		wantErr string
	}{
		{
			name:   "valid config wires the production adapters",
			mutate: func(*config) {},
		},
		{
			name:    "build command without the artifact token surfaces with the flag name",
			mutate:  func(cfg *config) { cfg.buildCommand = "go build ./cmd/template-mcp" },
			wantErr: "--" + buildFlag + ": the build command must reference " + artifactToken,
		},
		{
			name:    "negative debounce surfaces from the reloader core",
			mutate:  func(cfg *config) { cfg.debounce = -time.Millisecond },
			wantErr: "debounce must not be negative",
		},
		{
			name:    "negative quiesce surfaces from the reloader core",
			mutate:  func(cfg *config) { cfg.quiesce = -time.Millisecond },
			wantErr: "quiesce grace must not be negative",
		},
		{
			name:    "negative terminate surfaces from the upstream adapter",
			mutate:  func(cfg *config) { cfg.terminate = -time.Millisecond },
			wantErr: "terminate duration must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validProxyConfig()
			tt.mutate(&cfg)

			p, err := newProxy(cfg, strings.NewReader(""), io.Discard, io.Discard, discardLogger(), seams{})

			if tt.wantErr != "" {
				require.Error(t, err, "expected construction to fail")
				assert.ErrorContains(t, err, tt.wantErr, "expected the failure to name the cause")
				return
			}
			require.NoError(t, err, "expected construction to succeed without running anything")
			require.NotNil(t, p.core, "expected the reloader core to be wired")
			require.NotNil(t, p.frontend, "expected the downstream frontend to be wired")
			require.NotNil(t, p.transport, "expected a downstream transport over the command streams")
			assert.NoError(t, p.close(), "expected close to release the artifact directory")
		})
	}
}

func TestVersionFlagPrintsBuildMetadata(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	root := NewRootCommand(Options{
		Out: &stdout,
		Err: &stderr,
		Build: BuildInfo{
			Version: "0.1.0",
			Commit:  "abc1234",
			Date:    "2026-06-10T10:00:00Z",
		},
	})
	root.SetArgs([]string{"--version"})

	err := root.ExecuteContext(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "mcp-devproxy 0.1.0 (abc1234) built 2026-06-10T10:00:00Z\n", stdout.String())
	assert.Empty(t, stderr.String(), "version output must not write to stderr")
}
