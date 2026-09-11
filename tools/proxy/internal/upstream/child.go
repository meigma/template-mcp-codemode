package upstream

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// childSession is one live child MCP connection, implementing the
// reloader.ChildSession port on top of an mcp.ClientSession.
type childSession struct {
	session *mcp.ClientSession

	// tools is the validated snapshot taken by the health gate; immutable
	// once Start returns.
	tools []*mcp.Tool

	// toolsCh delivers re-listed, validated snapshots after the child emits
	// its own tools/list_changed. Capacity 1, written latest-wins.
	toolsCh chan []*mcp.Tool

	// done closes when the child dies unexpectedly — including when the
	// adapter declares it unhealthy after a failed runtime re-list; an
	// intentional Close never closes it.
	done chan struct{}

	// ready closes once session is set: the happens-before barrier letting
	// notification handlers use the session.
	ready chan struct{}

	// closing marks an intentional Close before the connection drops so the
	// done watcher never reports it as a crash.
	closing atomic.Bool

	logHandler    func(context.Context, *mcp.LoggingMessageParams)
	relistTimeout time.Duration
	logger        *slog.Logger
}

// Tools returns the validated tool snapshot taken by the health gate.
func (s *childSession) Tools() []*mcp.Tool { return s.tools }

// CallTool forwards one tool call to the child.
func (s *childSession) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return s.session.CallTool(ctx, params)
}

// ToolsChanged delivers a re-listed and validated tool snapshot each time
// the child emits its own tools/list_changed.
func (s *childSession) ToolsChanged() <-chan []*mcp.Tool { return s.toolsCh }

// Done is closed when the child dies unexpectedly.
func (s *childSession) Done() <-chan struct{} { return s.done }

// Close terminates the child session and, through the transport, its
// process. An intentional close is never reported as a crash on Done.
func (s *childSession) Close() error {
	s.closing.Store(true)
	return s.session.Close()
}

// watchDone turns the connection ending into crash detection for the core's
// supervisor: done closes unless the shutdown was an intentional Close.
func (s *childSession) watchDone() {
	_ = s.session.Wait()
	if !s.closing.Load() {
		close(s.done)
	}
}

// onToolListChanged re-lists the child's tools and publishes the validated
// snapshot. The synchronous re-list on the notification goroutine is safe:
// JSON-RPC responses bypass the handler queue, so the list call completes
// while this handler blocks, and the serialization of subsequent child
// notifications behind it is desirable ordering. A failed or invalid re-list
// means the child itself declared the advertised set stale and offered no
// trusted replacement, so routing against the old set would break the
// no-silent-execution-on-new-code guarantee: the child is treated as
// unhealthy — its session is closed without the intentional-Close mark, Done
// fires, and the core's crash supervision restarts it through the full
// health gate.
func (s *childSession) onToolListChanged(ctx context.Context, _ *mcp.ToolListChangedRequest) {
	select {
	case <-s.ready:
	default:
		// Pre-gate notification: the health gate's own list runs later and
		// captures the final set.
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, s.relistTimeout)
	defer cancel()

	tools, err := listTools(listCtx, s.session)
	if err != nil {
		s.logger.ErrorContext(ctx,
			"re-listing tools after the child's tools/list_changed failed; treating the child as unhealthy",
			"error", err)
		s.failUnhealthy()
		return
	}
	if err := reloader.ValidateTools(tools); err != nil {
		s.logger.ErrorContext(ctx,
			"child runtime tool change failed validation; treating the child as unhealthy",
			"error", err)
		s.failUnhealthy()
		return
	}
	s.publish(tools)
}

// failUnhealthy closes the child session without the intentional-Close mark,
// so watchDone reports the death on Done and the core's crash supervision
// restarts the child. The close runs on its own goroutine: the connection's
// Close waits for in-flight notification handlers to return, and this is
// called from one. An intentional Close already underway wins — Close is
// idempotent either way, and watchDone's closing check keeps a concurrent
// intentional Close from being reported as a crash. The unsynchronized
// closing check cannot double-fire from notification handlers: jsonrpc2
// serializes them, so two failUnhealthy calls never race each other — only
// an intentional Close, which the idempotent session Close absorbs.
func (s *childSession) failUnhealthy() {
	if s.closing.Load() {
		return
	}
	go func() { _ = s.session.Close() }()
}

// onLoggingMessage forwards one child log notification to the configured
// handler. It needs no session, so it safely runs pre-ready.
func (s *childSession) onLoggingMessage(ctx context.Context, req *mcp.LoggingMessageRequest) {
	if handler := s.logHandler; handler != nil {
		handler(ctx, req.Params)
	}
}

// publish replaces any unread snapshot with the newest one and never blocks.
// The core only consumes ToolsChanged while the child is the serving one, so
// a blocking send here would wedge the child's notification handling.
func (s *childSession) publish(tools []*mcp.Tool) {
	for {
		select {
		case s.toolsCh <- tools:
			return
		default:
		}
		select {
		case <-s.toolsCh:
		default:
		}
	}
}
