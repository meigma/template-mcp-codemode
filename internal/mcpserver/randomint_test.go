package mcpserver

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRandomInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      randomIntInput
		wantErr bool
	}{
		{name: "in range", in: randomIntInput{Min: 1, Max: 10}},
		{name: "equal bounds", in: randomIntInput{Min: 5, Max: 5}},
		{name: "negative range", in: randomIntInput{Min: -10, Max: -1}},
		{name: "spans zero", in: randomIntInput{Min: -3, Max: 3}},
		// Extreme ranges must not panic: a naive int64 span computation
		// (max - min + 1) overflows here and would make crypto/rand.Int panic.
		{name: "full int range", in: randomIntInput{Min: math.MinInt, Max: math.MaxInt}},
		{name: "wide range from min", in: randomIntInput{Min: math.MinInt, Max: 0}},
		{name: "wide range to max", in: randomIntInput{Min: 0, Max: math.MaxInt}},
		{name: "min greater than max", in: randomIntInput{Min: 10, Max: 1}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Repeat to exercise the random draw across the range.
			for range 100 {
				assertRandomInt(t, tt.in, tt.wantErr)
			}
		})
	}
}

// assertRandomInt runs randomInt once and checks the error and range
// expectations for the given input.
func assertRandomInt(t *testing.T, in randomIntInput, wantErr bool) {
	t.Helper()

	_, out, err := randomInt(context.Background(), nil, in)
	if wantErr {
		require.Error(t, err, "randomInt(%+v)", in)
		return
	}
	require.NoError(t, err, "randomInt(%+v)", in)
	assert.GreaterOrEqual(t, out.Value, in.Min, "randomInt(%+v)", in)
	assert.LessOrEqual(t, out.Value, in.Max, "randomInt(%+v)", in)
}
