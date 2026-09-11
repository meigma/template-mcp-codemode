// Package downstream is the driving adapter that owns the dev proxy's
// persistent client-facing MCP session.
//
// It implements the reloader.Frontend port on top of the go-sdk's mcp.Server
// served over stdio. The server advertises a superset capability envelope
// (tools, prompts, and resources with list_changed, plus logging) because the
// downstream session's capabilities freeze at its one initialize, even though
// v1 forwards tools only. Reconcile diffs the desired tool set against the
// advertised one by wire-definition fingerprint — removals through
// RemoveTools, additions and changes through one replacing non-generic
// AddTool each — and forwards calls byte-for-byte through the core's
// CallToolFunc. Receiving middleware gates per-session stale views: a call
// naming a tool that changed (or vanished) after that session's last
// tools/list is answered with the friendly stale-reload error until the
// session re-lists. The middleware also records the client's
// logging/setLevel so the upstream adapter can replay it to each new child.
package downstream
