package reloader

import (
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errShuttingDown fails tool calls when the proxy is shutting down, including
// calls still waiting in the swap buffer. It is a Go error rather than an
// IsError tool result because the downstream session is ending with the
// proxy: there is no next turn in which an LLM could read a result and
// self-correct.
var errShuttingDown = errors.New("dev proxy is shutting down")

// StaleReloadResult returns the friendly stale-reload error for a tool call
// issued against a definition that a dev reload has since changed or removed.
//
// It is an IsError tool result rather than a Go error so the message reaches
// the LLM's context and it can self-correct next turn — the SDK's raw
// ToolHandler path turns a handler's Go error into a protocol error the model
// never sees. The core answers buffered calls with it when the drain gate
// detects a changed or removed definition; it is exported because the
// downstream adapter's per-session stale-view gate returns the identical
// result for post-swap calls from sessions that have not re-listed.
func StaleReloadResult(name string) *mcp.CallToolResult {
	return errorResult(fmt.Sprintf("tool %q changed by dev reload; list refreshes next turn", name))
}

// supersededResult answers a call that was in flight on a child the swap
// replaced before the call completed. The call may have executed: a
// non-idempotent call must never be transparently replayed, so the caller is
// told to verify before retrying.
func supersededResult(name string) *mcp.CallToolResult {
	return errorResult(fmt.Sprintf(
		"tool %q call was interrupted by a dev reload and may or may not have executed; verify before retrying",
		name,
	))
}

// bufferOverflowResult answers a call rejected because the swap buffer is
// full. Erroring the excess call keeps the downstream session from ever
// blocking on an unbounded queue.
func bufferOverflowResult(name string) *mcp.CallToolResult {
	return errorResult(fmt.Sprintf(
		"tool %q call rejected: too many calls buffered during a dev reload; retry shortly",
		name,
	))
}

// bufferTimeoutResult answers a buffered call whose per-call timeout expired
// before a swap released it — the reload stalled, and the call must not wait
// forever.
func bufferTimeoutResult(name string) *mcp.CallToolResult {
	return errorResult(fmt.Sprintf(
		"tool %q call timed out waiting for a dev reload to finish; retry shortly",
		name,
	))
}

// errorResult wraps one message as an IsError tool result, the shape that
// reaches the LLM as visible tool output instead of a protocol error.
func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
