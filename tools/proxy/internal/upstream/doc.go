// Package upstream is the driven adapter that spawns and supervises one
// child MCP server at a time on behalf of the reloader core.
//
// It implements the reloader.Upstream and reloader.ChildSession ports on top
// of the go-sdk's mcp.Client. The default transport runs the built child
// artifact over stdio via mcp.CommandTransport with the child's stderr wired
// through to the proxy's stderr; tests inject in-memory transports through
// the TransportFactory seam. Every child is health-gated before it serves:
// its tools are listed under a timeout and each definition validated, and a
// child that fails the gate is killed while the core keeps the old one.
package upstream
