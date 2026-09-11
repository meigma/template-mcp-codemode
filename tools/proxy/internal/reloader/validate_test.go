package reloader

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateTools is a function-level table: most of these definitions are
// unreachable through a real SDK child (its own AddTool panics first), which
// is exactly why the upstream health gate needs the standalone check.
func TestValidateTools(t *testing.T) {
	t.Parallel()

	objectSchema := map[string]any{"type": "object"}

	tests := []struct {
		name    string
		tools   []*mcp.Tool
		wantErr string
	}{
		{
			name:  "valid tool with no output schema passes",
			tools: []*mcp.Tool{{Name: "echo", InputSchema: objectSchema}},
		},
		{
			name: "valid tool with an object output schema passes",
			tools: []*mcp.Tool{
				{Name: "echo", InputSchema: objectSchema, OutputSchema: map[string]any{"type": "object"}},
			},
		},
		{
			name:    "nil tool is rejected",
			tools:   []*mcp.Tool{nil},
			wantErr: "tool is nil",
		},
		{
			name:    "missing input schema is rejected",
			tools:   []*mcp.Tool{{Name: "echo"}},
			wantErr: "missing input schema",
		},
		{
			name:    "non-object input schema is rejected",
			tools:   []*mcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "array"}}},
			wantErr: `not "object"`,
		},
		{
			name:    "unmarshalable input schema is rejected",
			tools:   []*mcp.Tool{{Name: "echo", InputSchema: map[string]any{"bad": make(chan int)}}},
			wantErr: "not marshalable",
		},
		{
			name:    "non-object input schema document is rejected",
			tools:   []*mcp.Tool{{Name: "echo", InputSchema: []any{"not", "an", "object"}}},
			wantErr: "not a JSON object",
		},
		{
			name: "non-object output schema is rejected",
			tools: []*mcp.Tool{
				{Name: "echo", InputSchema: objectSchema, OutputSchema: map[string]any{"type": "string"}},
			},
			wantErr: "output schema",
		},
		{
			name:    "empty name is rejected",
			tools:   []*mcp.Tool{{Name: "", InputSchema: objectSchema}},
			wantErr: "name is empty",
		},
		{
			name:    "overlong name is rejected",
			tools:   []*mcp.Tool{{Name: strings.Repeat("a", 129), InputSchema: objectSchema}},
			wantErr: "128",
		},
		{
			name:    "name with invalid characters is rejected",
			tools:   []*mcp.Tool{{Name: "a b", InputSchema: objectSchema}},
			wantErr: "invalid character",
		},
		{
			name: "duplicate names are rejected even when individually valid",
			tools: []*mcp.Tool{
				{Name: "dup", InputSchema: objectSchema},
				{Name: "dup", InputSchema: objectSchema},
			},
			wantErr: "duplicate tool name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateTools(tt.tools)
			if tt.wantErr == "" {
				assert.NoError(t, err, "expected the tool set to validate")
				return
			}
			require.Error(t, err, "expected the tool set to be rejected")
			assert.Contains(t, err.Error(), tt.wantErr, "expected the rejection to say why")
		})
	}
}
