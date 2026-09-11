---
title: Add a tool
description: Replace the demo tool or add your own alongside it.
---

# Add a tool

Tools live in `internal/mcpserver`, one tool per file with a matching test file
(`randomint.go` / `randomint_test.go`). The transport and CLI code never change
when you add, replace, or remove a tool.

## The pattern

Each tool is a `registerXxx` function plus typed input/output structs and a
handler. `random_int` is the worked example:

```go
type randomIntInput struct {
	Min int `json:"min" jsonschema:"minimum value, inclusive"`
	Max int `json:"max" jsonschema:"maximum value, inclusive"`
}

type randomIntOutput struct {
	Value int `json:"value" jsonschema:"the generated random integer"`
}

func registerRandomInt(srv *mcp.Server, _ Dependencies) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "random_int",
		Description: "Return a random integer in the inclusive range [min, max].",
	}, randomInt)
}
```

The SDK derives the JSON Schemas from the `json`/`jsonschema` struct tags and
marshals the typed return value into the structured result automatically.

## Add a new tool

1. Create `internal/mcpserver/yourtool.go` with input/output structs, a
   `registerYourTool(srv *mcp.Server, deps Dependencies)` function, and a
   handler.
2. Call it from `New` in `internal/mcpserver/server.go`, next to
   `registerRandomInt(srv, options.Deps)`.
3. Add `internal/mcpserver/yourtool_test.go`, and add the new tool name to the
   expected tool set asserted in `server_test.go` so a missed registration fails
   CI.

## The tool-error convention

Return a regular `error` for an input the tool itself rejects (for example, an
out-of-range argument). The SDK turns it into a tool-level error result the model
can read and self-correct from — not a JSON-RPC protocol error:

```go
if in.Min > in.Max {
	return nil, yourOutput{}, fmt.Errorf("min (%d) must be <= max (%d)", in.Min, in.Max)
}
```

## Tools that need dependencies

A real tool often needs shared collaborators — a database handle, an outbound
HTTP client, a config struct. Add them to the `Dependencies` struct in
`server.go`, and the registration function receives them through `Options.Deps`:

```go
type Dependencies struct {
	DB *sql.DB
}

func registerLookup(srv *mcp.Server, deps Dependencies) {
	mcp.AddTool(srv, &mcp.Tool{Name: "lookup", /* ... */}, func(
		ctx context.Context, _ *mcp.CallToolRequest, in lookupInput,
	) (*mcp.CallToolResult, lookupOutput, error) {
		// use deps.DB here
	})
}
```

Construct the dependencies where the transports call `mcpserver.New(...)`
(`internal/cli/stdio.go` and `internal/cli/http.go`) and pass them as
`Options.Deps`. Because dependencies flow through `Options`, the server stays
transport-agnostic — both transports wire them the same way.

## Remove the demo tool

Delete `randomint.go` and `randomint_test.go`, remove the `registerRandomInt`
call in `server.go`, and update the expected tool set in `server_test.go`.
