// Package reloader is the dev proxy's core: it orchestrates the
// watch, build, health-gate, swap cycle that keeps a persistent downstream
// MCP session pointed at a disposable, rebuildable child server.
//
// The package is the center of the proxy's hexagonal architecture. It owns
// the ports at the external boundaries (file watching, building, child
// supervision, and the client-facing frontend) plus the pure orchestration
// logic: reconciling tool sets, buffering calls across swaps, and
// supervising child restarts. Transports, processes, and watchers live in
// adapter packages behind those ports. The mcp package's data types (tools,
// call params and results) are the domain vocabulary and are used directly.
package reloader
