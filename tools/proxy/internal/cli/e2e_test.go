// E2E: two Short-guarded tests driving the production exec and fsnotify
// adapters end to end — a real go build through the build adapter, real child
// processes via CommandTransport, a real fsnotify watch. They exist to prove
// those adapters (the reload loop and the shutdown escalation ladder), not
// the orchestration logic; reconciliation, stale gating, and logging
// passthrough are covered in-process by the integration suite. Only
// the downstream transport is injected: the fake Claude client needs an
// in-memory pair, exactly as a future HTTP downstream would slot into the
// same seam.
//
// unix-only: the no-orphans assertions probe processes with kill(pid, 0).

//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eBuildTimeout bounds each reload cycle's wait. The cold-start build may
// compile the SDK from an empty build cache in CI; later builds reuse it.
const e2eBuildTimeout = 120 * time.Second

// TestE2EReloadLoop proves the full dev loop over the real adapters in three
// phases: a cold start builds and serves the first child, a manifest write in
// the watched tree triggers rebuild → swap → re-advertise (and kills the old
// child process), and cancellation shuts everything down with no orphaned
// processes.
func TestE2EReloadLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: real go build and child processes; skipped with -short")
	}
	// The fixture must build offline by construction (tests never hit the
	// network); GOPROXY=off turns any violation into a build failure.
	// t.Setenv also keeps this test off the parallel schedule, so the env
	// change cannot leak into a concurrently running test.
	t.Setenv("GOPROXY", "off")

	watchDir := t.TempDir()
	manifest := filepath.Join(watchDir, "tools.json")
	// The pid file must live outside the watched tree: each child appends its
	// pid at startup, and a write inside watchDir would itself trigger
	// another reload cycle.
	pidFile := filepath.Join(t.TempDir(), "pids")

	writeManifest(t, manifest, `["alpha"]`)

	h := newE2EHarness(t, config{
		buildCommand: goTool(t) + " build -o {{artifact}} ./internal/cli/testdata/child",
		buildDir:     moduleRoot(t),
		watchDirs:    []string{watchDir},
		childArgv:    []string{"{{artifact}}", manifest, pidFile},
		debounce:     50 * time.Millisecond,
		quiesce:      time.Second,
		terminate:    time.Second,
	})

	// Phase 1 — cold start through the real adapters: the first build runs,
	// CommandTransport spawns the binary, the health gate passes, and the
	// manifest's tool is advertised and callable.
	h.awaitListChangedWithin(t, e2eBuildTimeout)
	require.Equal(t, []string{"alpha"}, h.listNames(t),
		"expected the first built child's manifest tool to be advertised")
	requireChildResult(t, h.callTool(t, "alpha"), "ok-alpha")
	require.Len(t, readPids(t, pidFile), 1, "expected exactly one child after cold start")

	// Phase 2 — a manifest write is a real fsnotify event: the proxy
	// rebuilds, swaps in the new process, re-advertises its tool set, and the
	// swapped-out child dies.
	h.drainListChanged()
	writeManifest(t, manifest, `["alpha","beta"]`)
	h.awaitListChangedWithin(t, e2eBuildTimeout)
	require.ElementsMatch(t, []string{"alpha", "beta"}, h.listNames(t),
		"expected the rebuilt child's tool set after the reload")
	requireChildResult(t, h.callTool(t, "beta"), "ok-beta")
	pids := readPids(t, pidFile)
	require.GreaterOrEqual(t, len(pids), 2, "expected the reload to have spawned a new child")
	requireProcessGone(t, pids[0])

	// Phase 3 — clean shutdown: cancellation classifies as a clean nil exit
	// (asserted inside shutdown) and no child process is orphaned.
	h.shutdown(t)
	for _, pid := range readPids(t, pidFile) {
		requireProcessGone(t, pid)
	}
}

// TestE2EHungChildEscalation proves the §5 failure row "old child hangs on
// Close" over the real CommandTransport: a child that survives stdin close
// and ignores SIGTERM must be SIGKILLed after --terminate per escalation
// step instead of stalling the proxy (shutdown here; a swap closes the old
// child through the same transport ladder). The marker file the fixture's
// hang mode writes on SIGTERM proves the ladder escalated in order: SIGTERM
// was delivered, ignored, and only then did SIGKILL end the process.
func TestE2EHungChildEscalation(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: real go build and child processes; skipped with -short")
	}
	t.Setenv("GOPROXY", "off")

	watchDir := t.TempDir()
	manifest := filepath.Join(watchDir, "tools.json")
	scratch := t.TempDir()
	pidFile := filepath.Join(scratch, "pids")
	sigtermMarker := filepath.Join(scratch, "sigterm-received")

	writeManifest(t, manifest, `["alpha"]`)

	// Short enough that both escalation steps fit comfortably inside the
	// harness's shutdown budget, long enough for the child's SIGTERM handler
	// to write the marker before SIGKILL lands.
	const terminate = 300 * time.Millisecond

	h := newE2EHarness(t, config{
		buildCommand: goTool(t) + " build -o {{artifact}} ./internal/cli/testdata/child",
		buildDir:     moduleRoot(t),
		watchDirs:    []string{watchDir},
		childArgv:    []string{"{{artifact}}", manifest, pidFile, sigtermMarker},
		debounce:     50 * time.Millisecond,
		quiesce:      time.Second,
		terminate:    terminate,
	})

	h.awaitListChangedWithin(t, e2eBuildTimeout)
	require.Equal(t, []string{"alpha"}, h.listNames(t),
		"expected the hang-mode child to serve normally until shutdown")
	pids := readPids(t, pidFile)
	require.Len(t, pids, 1, "expected exactly one child after cold start")

	// shutdown asserts the clean nil exit within waitTimeout: the hung child
	// must cost at most the two escalation waits, never a stall.
	h.shutdown(t)

	requireProcessGone(t, pids[0])
	marker, err := os.ReadFile(sigtermMarker)
	require.NoError(t, err,
		"expected the child to have recorded the SIGTERM it ignored before SIGKILL ended it")
	assert.Contains(t, string(marker), "sigterm", "expected the marker to record the ignored SIGTERM")
}

// newE2EHarness wires the proxy with the production seams — the real fsnotify
// watcher, exec builder, and CommandTransport children — injecting only the
// in-memory downstream transport for the fake Claude client. The proxy's
// stderr (its logs plus every child's stderr passthrough) is captured and
// dumped when the test fails.
func newE2EHarness(t *testing.T, cfg config) *integrationHarness {
	t.Helper()

	h := &integrationHarness{
		listChanged: make(chan struct{}, 16),
		logs:        make(chan *mcp.LoggingMessageParams, 16),
		runErr:      make(chan error, 1),
	}
	h.ctx, h.cancel = context.WithCancel(t.Context())

	stderr := &syncBuffer{}
	// Registered before start's shutdown cleanup so it runs after everything
	// is down and the buffer is complete.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("proxy stderr:\n%s", stderr.String())
		}
	})
	h.start(t, cfg, seams{}, stderr, slog.New(slog.NewTextHandler(stderr, nil)))
	return h
}

// syncBuffer is a mutex-guarded buffer: the proxy's logger and child stderr
// passthrough write to it from proxy goroutines while the test goroutine
// reads it on failure.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// goTool returns the go command of the toolchain that compiled this test
// binary. The bare "go" on PATH may be a version manager's shim for a
// different toolchain; GOROOT's is the one whose module graph built the test,
// which is also what keeps the fixture build offline.
func goTool(t *testing.T) string {
	t.Helper()

	//nolint:staticcheck // Deprecation suggests "go env GOROOT", which would trust exactly the PATH lookup being avoided.
	tool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(tool); err != nil {
		t.Skipf("no go tool under GOROOT: %v", err)
	}
	return tool
}

// moduleRoot returns this module's root directory — the build command must
// run where go.mod lives, two levels above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err, "resolve the module root")
	return root
}

// writeManifest (re)writes the child fixture's tool manifest — the E2E
// stand-in for saving a source change in the watched tree.
func writeManifest(t *testing.T, path, tools string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(tools), 0o600), "write the tools manifest")
}

// readPids parses the pid file the child fixture appends to at startup: one
// pid per line, oldest child first.
func readPids(t *testing.T, path string) []int {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read the child pid file")
	fields := strings.Fields(string(raw))
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		require.NoError(t, err, "parse pid entry %q", field)
		pids = append(pids, pid)
	}
	return pids
}

// requireProcessGone asserts the process exits (and is reaped) shortly.
// Signal 0 probes existence without signaling; ESRCH means gone — a zombie
// still answers, so this also proves the transport reaped the child.
func requireProcessGone(t *testing.T, pid int) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, waitTimeout, 50*time.Millisecond, "expected child process %d to be dead", pid)
}
