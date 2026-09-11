package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/template-mcp/tools/proxy/internal/build"
	"github.com/meigma/template-mcp/tools/proxy/internal/downstream"
	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
	"github.com/meigma/template-mcp/tools/proxy/internal/upstream"
	"github.com/meigma/template-mcp/tools/proxy/internal/watch"
)

// seams carries test-only adapter overrides; the zero value selects the
// production adapters. Integration and E2E tests inject fakes here so they
// exercise newProxy's real construction order instead of a parallel assembly
// that could drift.
type seams struct {
	// watcher overrides the fsnotify watch adapter.
	watcher reloader.Watcher

	// builder overrides the exec build adapter.
	builder reloader.Builder

	// childTransport overrides the upstream adapter's CommandTransport
	// factory, so tests can inject in-memory children.
	childTransport upstream.TransportFactory

	// downstreamTransport overrides the transport the downstream session is
	// served over; nil selects an IOTransport over the command streams.
	downstreamTransport mcp.Transport
}

// proxy bundles the constructed core, adapters, and downstream transport.
// run drives the two loops; close releases what construction acquired.
type proxy struct {
	core         *reloader.Reloader
	frontend     *downstream.Frontend
	transport    mcp.Transport
	input        *eofReader
	closeBuilder func() error
}

// newProxy wires the dev proxy in its required construction order: core
// adapters first, then the reloader core, then SetFrontend before any Run.
// The logging passthrough is wired adapter-to-adapter on purpose — the
// downstream frontend's last-known level feeds each new child, and child log
// messages feed every downstream session — because the core orchestrates
// reloads, not log lines.
func newProxy(
	cfg config,
	in io.Reader,
	out, errOut io.Writer,
	logger *slog.Logger,
	s seams,
) (*proxy, error) {
	p := &proxy{}
	fail := func(err error) (*proxy, error) {
		_ = p.close()
		return nil, err
	}

	builder := s.builder
	if builder == nil {
		b, err := build.New(build.Options{Command: cfg.buildCommand, Dir: cfg.buildDir, Logger: logger})
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", buildFlag, err)
		}
		builder = b
		p.closeBuilder = b.Close
	}

	watcher := s.watcher
	if watcher == nil {
		w, err := watch.New(watch.Options{Dirs: cfg.watchDirs, Logger: logger})
		if err != nil {
			return fail(fmt.Errorf("--%s: %w", watchFlag, err))
		}
		watcher = w
	}

	frontend, err := downstream.New(downstream.Options{Logger: logger})
	if err != nil {
		return fail(fmt.Errorf("construct downstream frontend: %w", err))
	}
	p.frontend = frontend

	up, err := upstream.New(upstream.Options{
		Argv:              cfg.childArgv,
		TerminateDuration: cfg.terminate,
		LogHandler:        frontend.Log,
		LevelProvider:     frontend.Level,
		// Child stderr is forwarded to the proxy's stderr; the SDK's
		// CommandTransport does not wire it.
		Stderr:    errOut,
		Transport: s.childTransport,
		Logger:    logger,
	})
	if err != nil {
		return fail(fmt.Errorf("construct upstream: %w", err))
	}

	core, err := reloader.New(reloader.Options{
		Watcher:      watcher,
		Builder:      builder,
		Upstream:     up,
		Logger:       logger,
		Debounce:     cfg.debounce,
		QuiesceGrace: cfg.quiesce,
	})
	if err != nil {
		return fail(fmt.Errorf("configure reloader core: %w", err))
	}
	core.SetFrontend(frontend)
	p.core = core

	p.input = &eofReader{reader: in}
	p.transport = s.downstreamTransport
	if p.transport == nil {
		p.transport = &mcp.IOTransport{
			Reader: io.NopCloser(p.input),
			Writer: nopWriteCloser{Writer: out},
		}
	}
	return p, nil
}

// run serves the downstream session and drives the reload core until either
// side stops, with mutual cancellation: the client closing its end tears the
// core (and every child it owns) down, and a core failure tears the session
// down. Clean shutdowns — context cancellation from a signal, or the client
// closing the input stream — return nil; anything else is a real failure.
func (p *proxy) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	frontendErr := make(chan error, 1)
	go func() {
		frontendErr <- p.frontend.Run(ctx, p.transport)
		cancel()
	}()

	coreErr := p.core.Run(ctx)
	cancel()
	// Wait for the frontend before returning so the caller's cleanup runs
	// only after both loops are down; the core's Run has already closed
	// every child by the time it returns.
	feErr := <-frontendErr

	// The core returns nil on ctx cancellation, so a non-nil coreErr is a
	// real failure (an unwatchable source tree, a dead watcher) regardless
	// of which side stopped first.
	if coreErr != nil {
		return coreErr
	}
	// Same classification as the template server's stdio command: a
	// cancelled context or the client closing the input stream is a clean
	// exit; the SDK can surface the latter as a non-EOF "server is closing"
	// error, which sawEOF recognizes.
	switch {
	case feErr == nil, errors.Is(feErr, context.Canceled), p.input.sawEOF():
		return nil
	default:
		return feErr
	}
}

// close releases resources construction acquired outside run: today only the
// production build adapter's per-cycle artifact directory.
func (p *proxy) close() error {
	if p.closeBuilder == nil {
		return nil
	}
	return p.closeBuilder()
}

// launchProxy is the production launchFunc: it wires the real adapters over
// the command's streams and serves until the command context is cancelled or
// the client disconnects.
func launchProxy(cmd *cobra.Command, cfg config, logger *slog.Logger) error {
	ctx := cmd.Context()
	p, err := newProxy(cfg, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), logger, seams{})
	if err != nil {
		return err
	}
	defer func() {
		if err := p.close(); err != nil {
			logger.WarnContext(ctx, "removing the artifact directory failed", "error", err)
		}
	}()
	return p.run(ctx)
}
