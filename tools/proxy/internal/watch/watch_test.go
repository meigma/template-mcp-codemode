package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
	"github.com/meigma/template-mcp/tools/proxy/internal/watch"
)

const eventTimeout = 5 * time.Second

type testContext struct {
	dir    string
	events <-chan reloader.ChangeEvent
	cancel context.CancelFunc
}

// newTestContext builds a watcher over a fresh tempdir (prepared by before
// when non-nil) and starts watching it.
func newTestContext(t *testing.T, before func(t *testing.T, dir string)) *testContext {
	t.Helper()

	// macOS tempdirs live behind a /var → /private/var symlink; resolve it so
	// event paths compare equal to the paths the test constructs.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "resolve tempdir symlinks")

	if before != nil {
		before(t, dir)
	}

	w, err := watch.New(watch.Options{Dirs: []string{dir}})
	require.NoError(t, err, "construct watcher")

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	events, err := w.Watch(ctx)
	require.NoError(t, err, "start watching")

	return &testContext{dir: dir, events: events, cancel: cancel}
}

// receiveEvent returns the next forwarded change event.
func (tc *testContext) receiveEvent(t *testing.T) reloader.ChangeEvent {
	t.Helper()

	select {
	case ev, ok := <-tc.events:
		require.True(t, ok, "events channel closed before an event arrived")
		return ev
	case <-time.After(eventTimeout):
		t.Fatal("timed out waiting for a change event")
		return reloader.ChangeEvent{}
	}
}

// receiveEventForPath consumes events until one carries path.
func (tc *testContext) receiveEventForPath(t *testing.T, path string) {
	t.Helper()

	deadline := time.After(eventTimeout)
	for {
		select {
		case ev, ok := <-tc.events:
			require.True(t, ok, "events channel closed while waiting for an event for %s", path)
			if ev.Path == path {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a change event for %s", path)
		}
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o600), "write %s", path)
}

func TestWatcherWatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		before func(t *testing.T, dir string)
		act    func(t *testing.T, tc *testContext)
	}{
		{
			name: "write to an existing file emits its path and a preceding chmod is filtered",
			before: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "a.txt"))
				writeFile(t, filepath.Join(dir, "b.txt"))
			},
			act: func(t *testing.T, tc *testContext) {
				require.NoError(t, os.Chmod(filepath.Join(tc.dir, "b.txt"), 0o755))
				writeFile(t, filepath.Join(tc.dir, "a.txt"))

				ev := tc.receiveEvent(t)
				assert.Equal(t, filepath.Join(tc.dir, "a.txt"), ev.Path,
					"expected the write event, not the preceding chmod, to be the first event forwarded")
			},
		},
		{
			name: "file created in a pre-existing nested subdirectory emits an event",
			before: func(t *testing.T, dir string) {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", "inner"), 0o750))
			},
			act: func(t *testing.T, tc *testContext) {
				path := filepath.Join(tc.dir, "sub", "inner", "new.txt")
				writeFile(t, path)
				tc.receiveEventForPath(t, path)
			},
		},
		{
			name: "directory created after Watch is watched recursively",
			act: func(t *testing.T, tc *testContext) {
				newDir := filepath.Join(tc.dir, "newdir")
				require.NoError(t, os.Mkdir(newDir, 0o750))

				// Receiving the directory's own event proves the loop already
				// registered it (the adapter registers before forwarding), so
				// the write below cannot race the recursive add.
				tc.receiveEventForPath(t, newDir)

				path := filepath.Join(newDir, "inside.txt")
				writeFile(t, path)
				tc.receiveEventForPath(t, path)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := newTestContext(t, tt.before)
			tt.act(t, tc)
		})
	}
}

func TestWatcherWatchCancelClosesChannel(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t, nil)
	tc.cancel()

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-tc.events:
			return !ok
		default:
			return false
		}
	}, eventTimeout, 10*time.Millisecond, "expected the events channel to close after cancellation")
}

func TestWatcherWatchNonexistentDir(t *testing.T) {
	t.Parallel()

	w, err := watch.New(watch.Options{Dirs: []string{filepath.Join(t.TempDir(), "missing")}})
	require.NoError(t, err, "construct watcher")

	events, err := w.Watch(t.Context())
	require.Error(t, err, "expected Watch to fail for a nonexistent directory")
	assert.Nil(t, events, "expected no events channel on Watch failure")
}

func TestNewRequiresDirs(t *testing.T) {
	t.Parallel()

	_, err := watch.New(watch.Options{})
	require.Error(t, err, "expected New to reject an empty directory list")
}
