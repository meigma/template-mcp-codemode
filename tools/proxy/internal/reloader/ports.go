package reloader

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Watcher reports source-change events. Implementations send raw events;
// debouncing/coalescing is core logic, not the adapter's job (it must be
// unit-testable).
type Watcher interface {
	// Watch streams change events until ctx is cancelled.
	Watch(ctx context.Context) (<-chan ChangeEvent, error)
}

// Builder produces a runnable child artifact from the source tree.
type Builder interface {
	// Build runs one build. A non-nil error means "keep the old child".
	// The BuildResult carries the unique artifact path for this cycle and
	// any compile output for surfacing to the developer.
	//
	// Build must honor ctx and return promptly once it is cancelled
	// (typically with ctx.Err()). The core's cancel-and-supersede and
	// shutdown paths block until the in-flight cycle's Build or Start
	// returns, so an implementation that ignores cancellation stalls
	// reloads and shutdown alike.
	Build(ctx context.Context) (BuildResult, error)
}

// Upstream spawns and supervises one child MCP session at a time.
type Upstream interface {
	// Start launches the artifact, connects, initializes, health-gates it
	// (ListTools under timeout), and validates every advertised tool
	// definition (schema present, object-typed, marshalable — the
	// downstream AddTool panics otherwise). Invalid tools fail the gate.
	// A non-nil error means "keep the old child".
	//
	// Start must honor ctx and return promptly once it is cancelled —
	// either with an error after tearing down the half-started child, or
	// with the live session, which the core then closes. The core's
	// cancel-and-supersede and shutdown paths block until the in-flight
	// cycle's Build or Start returns, so an implementation that ignores
	// cancellation stalls reloads and shutdown alike.
	Start(ctx context.Context, artifact string) (ChildSession, error)
}

// ChildSession is one live child MCP connection.
type ChildSession interface {
	// Tools returns the validated tool snapshot taken by the health gate.
	Tools() []*mcp.Tool
	// CallTool forwards one tool call to the child.
	CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
	// ToolsChanged delivers a re-listed and validated tool snapshot each
	// time the child emits its own tools/list_changed.
	ToolsChanged() <-chan []*mcp.Tool
	// Done is closed when the child dies unexpectedly — including when the
	// adapter declares it unhealthy because a runtime re-list after the
	// child's own tools/list_changed failed or validated invalid. An
	// intentional Close never closes it.
	Done() <-chan struct{}
	// Close terminates the child session and its process.
	Close() error
}

// Frontend is the client-facing side the core drives. The downstream adapter
// implements it on top of mcp.Server: removed tools via RemoveTools, added
// and changed tools via a single replacing AddTool each — which is what
// emits the (coalesced) tools/list_changed. There is no Remove+Add dance:
// AddTool replaces in place, the notification has no payload, and clients
// refetch the same final list either way; a Remove+Add would only open a
// window in which the tool transiently does not exist.
type Frontend interface {
	// Reconcile makes the advertised tool set match tools, wiring each
	// tool's handler to call. It returns an error instead of panicking on a
	// definition that slipped past validation. A no-op diff makes no
	// AddTool/RemoveTools calls and emits nothing.
	Reconcile(tools []*mcp.Tool, call CallToolFunc) error
}

// CallToolFunc routes one forwarded tool call; the core provides its router
// method, which targets the current ChildSession (or the swap buffer).
//
// Forwarding conversion (v1): the router constructs fresh CallToolParams
// carrying only Name and the raw Arguments bytes (byte-for-byte, no
// validation or defaulting). Meta is dropped entirely — including the
// progressToken, which lives in _meta: forwarding it would invite progress
// notifications the proxy does not relay in v1. Cancellation still
// propagates via ctx.
type CallToolFunc func(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
