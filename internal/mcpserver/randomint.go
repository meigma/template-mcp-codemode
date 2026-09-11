package mcpserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
)

// randomIntName is the dotted Starlark name of the demo capability.
const randomIntName = "random.int"

// randomIntInput is the typed input for random.int. JSON tags name the
// keyword arguments; CodeMode accepts only json tags on int64 fields.
type randomIntInput struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// randomIntOutput is the typed output for random.int.
type randomIntOutput struct {
	Value int64 `json:"value"`
}

// registerRandomInt adds the random.int capability to the builder. It accepts
// the server's [Dependencies] to model the wiring real capabilities use — a
// handler that needs a database or HTTP client would close over deps here —
// even though random.int itself needs none.
func registerRandomInt(builder *codemode.Builder, _ Dependencies) {
	codemode.Register(builder, codemode.Capability[randomIntInput, randomIntOutput]{
		Name: randomIntName,
		Summary: "Return a cryptographically uniform random integer in the " +
			"inclusive range [min, max].",
		Handler: randomInt,
	})
}

// randomInt generates a uniformly random integer in [in.Min, in.Max].
//
// It draws from crypto/rand rather than math/rand: this is a security-conscious
// default for a template others will copy. Consumers that do not need
// unpredictable values can substitute math/rand.
func randomInt(
	_ context.Context,
	_ authz.Subject,
	in randomIntInput,
) (randomIntOutput, error) {
	if in.Min > in.Max {
		return randomIntOutput{}, fmt.Errorf("min (%d) must be <= max (%d)", in.Min, in.Max)
	}

	// span is the size of the half-open interval [0, span) to draw from, i.e.
	// max - min + 1. It is computed with big.Int throughout: int64 arithmetic
	// would overflow for client-controlled extreme ranges (for example
	// min=math.MinInt64, max=math.MaxInt64), wrapping to a non-positive value
	// that makes crypto/rand.Int panic.
	span := new(big.Int).Sub(big.NewInt(in.Max), big.NewInt(in.Min))
	span.Add(span, big.NewInt(1))

	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return randomIntOutput{}, fmt.Errorf("generate random int: %w", err)
	}

	// Shift the [0, span) draw into the inclusive range [min, max]. The result
	// is guaranteed to lie within [min, max], so it fits back into an int64
	// and Int64 cannot overflow.
	value := new(big.Int).Add(n, big.NewInt(in.Min))

	return randomIntOutput{Value: value.Int64()}, nil
}
