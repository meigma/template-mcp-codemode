package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// eventBufferSize is the fsnotify event buffer size; it absorbs editor save
// storms between reads of the Events channel.
const eventBufferSize = 64

// overflowPath is the synthetic ChangeEvent path emitted when fsnotify
// reports an event-queue overflow: a dropped event must still trigger a
// rebuild.
const overflowPath = "<overflow>"

// relevantOps are the fsnotify operations forwarded as change events.
// Everything else (pure Chmod) is noise; macOS deletes emit Chmod followed
// by Remove, so filtering Chmod loses nothing.
const relevantOps = fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename

// Options configures a Watcher for New.
type Options struct {
	// Dirs lists the directories to watch recursively. Required, non-empty.
	Dirs []string

	// Logger receives operational logs. Nil selects a no-op logger.
	Logger *slog.Logger
}

// Watcher is the fsnotify-backed implementation of the reloader.Watcher
// port. It watches the configured directories recursively, registering
// newly created subdirectories as they appear, and forwards raw change
// events; debouncing is the reloader core's job.
type Watcher struct {
	dirs   []string
	logger *slog.Logger
}

// New constructs a Watcher from options.
//
// New fails when Dirs is empty; the paths themselves are not inspected here.
// The fsnotify watcher is created per Watch call, so a missing or unreadable
// directory fails Watch, not New.
func New(options Options) (*Watcher, error) {
	if len(options.Dirs) == 0 {
		return nil, errors.New("at least one watch directory is required")
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Watcher{dirs: options.Dirs, logger: logger}, nil
}

// Watch begins streaming raw change events for the configured directories.
//
// The returned channel closes when ctx is cancelled (clean shutdown) or when
// the underlying fsnotify watcher dies (which the core surfaces as an
// unexpected close). Watch fails if any configured directory cannot be
// walked and registered.
func (w *Watcher) Watch(ctx context.Context) (<-chan reloader.ChangeEvent, error) {
	fsw, err := fsnotify.NewBufferedWatcher(eventBufferSize)
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	for _, dir := range w.dirs {
		if err := addRecursive(fsw, dir); err != nil {
			_ = fsw.Close()
			return nil, err
		}
	}

	out := make(chan reloader.ChangeEvent, 1)
	go w.loop(ctx, fsw, out)
	return out, nil
}

// loop forwards fsnotify events until ctx is cancelled or fsnotify dies.
// Closing out without ctx cancellation makes the core report the watcher
// failure instead of treating it as a clean shutdown.
func (w *Watcher) loop(ctx context.Context, fsw *fsnotify.Watcher, out chan<- reloader.ChangeEvent) {
	defer close(out)
	defer func() { _ = fsw.Close() }()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-fsw.Events:
			if !ok || !w.handleEvent(ctx, fsw, out, ev) {
				return
			}

		case err, ok := <-fsw.Errors:
			if !ok || !w.handleError(ctx, out, err) {
				return
			}
		}
	}
}

// handleEvent filters one fsnotify event, registers newly created
// directories, and forwards the event. It reports false when ctx was
// cancelled mid-send.
func (w *Watcher) handleEvent(
	ctx context.Context,
	fsw *fsnotify.Watcher,
	out chan<- reloader.ChangeEvent,
	ev fsnotify.Event,
) bool {
	if !ev.Op.Has(relevantOps) {
		return true
	}

	// Register before forwarding: once the directory's own event has been
	// observed downstream, files created inside it are already watched.
	if ev.Op.Has(fsnotify.Create) {
		w.addCreatedDir(ctx, fsw, ev.Name)
	}

	return w.forward(ctx, out, reloader.ChangeEvent{Path: ev.Name})
}

// handleError logs one fsnotify error and, on event-queue overflow, emits a
// synthetic change event so the dropped event still triggers a rebuild. It
// reports false when ctx was cancelled mid-send.
func (w *Watcher) handleError(ctx context.Context, out chan<- reloader.ChangeEvent, err error) bool {
	w.logger.ErrorContext(ctx, "fsnotify watcher error", "error", err)

	if errors.Is(err, fsnotify.ErrEventOverflow) {
		return w.forward(ctx, out, reloader.ChangeEvent{Path: overflowPath})
	}

	return true
}

// forward sends one change event, abandoning the send if ctx is cancelled
// first. It reports whether the event was delivered.
func (w *Watcher) forward(ctx context.Context, out chan<- reloader.ChangeEvent, ev reloader.ChangeEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// addCreatedDir registers a newly created directory (and anything beneath
// it) for watching. Non-directories and already-vanished paths are skipped;
// registration failures are logged and the loop continues.
func (w *Watcher) addCreatedDir(ctx context.Context, fsw *fsnotify.Watcher, path string) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}

	if err := addRecursive(fsw, path); err != nil {
		w.logger.ErrorContext(ctx, "watch newly created directory", "path", path, "error", err)
	}
}

// addRecursive registers dir and every directory beneath it with fsw.
// fsnotify watches single directories only, so the adapter walks the tree
// (the root is added before its children are visited).
//
// A file created inside a brand-new directory before this walk registers it
// produces no event of its own. That is safe by construction: the
// directory's own Create event was already forwarded, the core debounces,
// and the build reads the tree from disk after the debounce — so the file is
// picked up without an event.
func addRecursive(fsw *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if !d.IsDir() {
			return nil
		}
		if err := fsw.Add(path); err != nil {
			return fmt.Errorf("watch %s: %w", path, err)
		}
		return nil
	})
}
