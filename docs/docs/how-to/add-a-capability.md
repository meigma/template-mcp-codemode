---
title: Add a capability
description: Register a typed Go capability and remove the random.int demo.
---

# Add a capability

Capabilities live in `internal/mcpserver`. The CLI and transports continue to expose only `search_api`, `describe_api`, and `execute`; adding a capability changes the catalog behind those fixed MCP tools.

This guide adds `text.uppercase`, verifies how an agent composes it with `random.int`, and then explains how to remove the demo capability.

## Define the capability

Create `internal/mcpserver/uppercase.go`:

```go
package mcpserver

import (
	"context"
	"strings"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
)

type uppercaseInput struct {
	Value string `json:"value"`
}

type uppercaseOutput struct {
	Value string `json:"value"`
}

func registerUppercase(builder *codemode.Builder, _ Dependencies) {
	codemode.Register(builder, codemode.Capability[uppercaseInput, uppercaseOutput]{
		ID:          "text.value.uppercase",
		Name:        "text.uppercase",
		Summary:     "Convert text to uppercase.",
		Description: "Return the supplied text with Unicode letters mapped to uppercase.",
		SearchTerms: []string{"capitalize text", "change letter case"},
		Handler:     uppercase,
	})
}

func uppercase(
	_ context.Context,
	_ authz.Subject,
	in uppercaseInput,
) (uppercaseOutput, error) {
	return uppercaseOutput{Value: strings.ToUpper(in.Value)}, nil
}
```

The exported struct fields and `json` tags define the callable input and result shape. CodeMode does not use `jsonschema` tags for capability descriptions; `Summary`, `Description`, and `SearchTerms` provide discovery text.

Use only field types supported by CodeMode. Input structs must be non-pointer structs with direct exported fields. Scalar input fields are `string`, `int64`, `bool`, `float64`, or pointers to those types. Integers are signed 64-bit values: use `int64`, not `int`. JSON tags can rename fields and mark supported pointer fields with `omitempty`; unrelated struct tags are rejected. Output structs support additional recursive shapes. See the [canonical supported-types reference](https://meigma.github.io/codemode/reference/public-api/#supported-input-and-output-types) instead of copying the entire matrix into this repository.

Set an explicit stable `ID` before policy or deployment filters depend on a capability. `Name` is the dotted Starlark name shown through discovery. `SearchTerms` affect search only; they are not callable aliases and must not contain secrets or tenant-sensitive data.

## Register it in the runtime

In `internal/mcpserver/server.go`, call the registration after the builder is created and before `Build`:

```go
builder := codemode.New(options.Runtime)
registerRandomInt(builder, options.Deps)
registerUppercase(builder, options.Deps)
service, err := builder.Build()
```

Keep dependencies flowing through `Options.Deps`. Do not construct them in the registration function, a transport session factory, or a handler call.

`Builder.Build` validates all registered metadata and type shapes, applies static capability filtering and limits, probes the worker entry point, and returns one immutable runtime. `internal/mcpserver.New` then adapts that runtime through the required resolver. It returns `(*mcp.Server, error)`, so every transport caller must handle construction failure.

Do not call `mcp.AddTool` for `text.uppercase`. A direct MCP registration would create a second public surface beside CodeMode and would bypass its discovery, authorization, execution, and worker contracts.

## Keep the worker entry point in tests

Every test binary that calls `Builder.Build` must serve CodeMode worker mode before test setup. Add one `TestMain` to the applicable package if it does not already have one:

```go
func TestMain(m *testing.M) {
	codemode.ServeWorkerAndExit()
	os.Exit(m.Run())
}
```

`codemode.ServeWorkerAndExit()` must be the first statement. The production binary has the same requirement in `main`.

Test the observable handler contract: discovery metadata and shape where relevant, successful output, meaningful boundary behavior, authorization, and real error cases. Avoid tests that only assert that fields were copied or a registration call exists.

## Compose the new capability

After rebuilding or after the development proxy completes a reload:

1. Call `search_api` with `{"query":"change letter case"}`.
2. Pass the exact returned name `text.uppercase` to `describe_api`.
3. Run this source through `execute`:

```python
def main():
    draw = random.int(min=7, max=7)
    label = text.uppercase(value="draw " + str(draw["value"]))
    return {
        "draw": draw["value"],
        "label": label["value"],
    }
```

A capability output struct becomes a Starlark dictionary, so this program reads both results through their `"value"` keys. The successful structured MCP result is:

```json
{"result":{"draw":7,"label":"DRAW 7"}}
```

Capability-only edits do not change the outer definitions of `search_api`, `describe_api`, and `execute`. The development proxy therefore does not promise a `notifications/tools/list_changed` notification for this change. Verify the new child by repeating search, description, and execution and checking the returned capability result.

## Use shared dependencies

Add database pools, HTTP clients, clocks, or other shared collaborators as typed fields on `Dependencies` in `internal/mcpserver/server.go`. The registration function receives `Dependencies`; close over only the fields its handler needs.

Construct `Dependencies` once in the CLI startup path and pass the same value through `mcpserver.Options` for either transport. The HTTP server shares the immutable runtime and its collaborators across sessions. Dependencies and handlers must be safe for concurrent calls.

Handlers run in the privileged parent process, not in the Starlark worker. They must honor context cancellation for I/O, waits, locks, and downstream calls; bound their own resource use; and avoid exposing credentials or trusted diagnostic detail in returned values. CodeMode can kill the worker but cannot forcibly stop a dispatched Go handler or undo its side effects.

## Remove `random.int`

Add and verify at least one real capability first. Then:

1. Delete `internal/mcpserver/randomint.go` and its behavior tests.
2. Remove `registerRandomInt(builder, options.Deps)` from `internal/mcpserver.New`.
3. Update catalog expectations and documentation to describe the real capabilities.
4. Use a client to search, describe, and execute a replacement capability over every retained transport.

Keep the CodeMode builder, `mcpserver.Options.Runtime`, resolver wiring, and worker entry points. Removing the demo does not turn replacement capabilities into direct MCP tools.
