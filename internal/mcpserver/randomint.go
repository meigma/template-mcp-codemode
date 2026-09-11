package mcpserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// randomIntToolName is the registered name of the random_int tool, surfaced to
// clients via tools/list and tools/call.
const randomIntToolName = "random_int"

// randomIntInput is the typed input for the random_int tool. The json tags name
// the JSON Schema properties and the jsonschema tags supply their descriptions;
// the SDK derives the tool's inputSchema from this struct automatically.
type randomIntInput struct {
	Min int `json:"min" jsonschema:"minimum value, inclusive"`
	Max int `json:"max" jsonschema:"maximum value, inclusive"`
}

// randomIntOutput is the typed output for the random_int tool. The SDK derives
// the tool's outputSchema from this struct and marshals the value into the
// CallToolResult.StructuredContent field automatically.
type randomIntOutput struct {
	Value int `json:"value" jsonschema:"the generated random integer"`
}

// registerRandomInt adds the random_int tool to the server. It accepts the
// server's [Dependencies] to model the wiring real tools use — a handler that
// needs a database or HTTP client would close over deps here — even though
// random_int itself needs none.
func registerRandomInt(srv *mcp.Server, _ Dependencies) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: randomIntToolName,
		Description: "Return a cryptographically uniform random integer in the " +
			"inclusive range [min, max]. Returns a tool error if min > max.",
	}, randomInt)
}

// randomInt generates a uniformly random integer in [in.Min, in.Max].
//
// It draws from crypto/rand rather than math/rand: this is a security-conscious
// default for a template others will copy. Consumers that do not need
// unpredictable values can substitute math/rand.
func randomInt(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in randomIntInput,
) (*mcp.CallToolResult, randomIntOutput, error) {
	if in.Min > in.Max {
		// Returning a regular error makes the SDK populate
		// CallToolResult.IsError, i.e. a tool-level error result the model can
		// see and self-correct from, NOT a JSON-RPC protocol error. This is the
		// MCP tool-error convention.
		return nil, randomIntOutput{}, fmt.Errorf("min (%d) must be <= max (%d)", in.Min, in.Max)
	}

	// span is the size of the half-open interval [0, span) to draw from, i.e.
	// max - min + 1. It is computed with big.Int throughout: int64 arithmetic
	// would overflow for client-controlled extreme ranges (for example
	// min=math.MinInt, max=math.MaxInt), wrapping to a non-positive value that
	// makes crypto/rand.Int panic.
	span := new(big.Int).Sub(big.NewInt(int64(in.Max)), big.NewInt(int64(in.Min)))
	span.Add(span, big.NewInt(1))

	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return nil, randomIntOutput{}, fmt.Errorf("generate random int: %w", err)
	}

	// Shift the [0, span) draw into the inclusive range [min, max]. The result is
	// guaranteed to lie within [min, max], so it fits back into an int and Int64
	// cannot overflow.
	value := new(big.Int).Add(n, big.NewInt(int64(in.Min)))

	return nil, randomIntOutput{Value: int(value.Int64())}, nil
}
