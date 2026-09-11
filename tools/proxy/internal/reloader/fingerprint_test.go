package reloader

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutateA   func(tool *mcp.Tool)
		mutateB   func(tool *mcp.Tool)
		wantEqual bool
	}{
		{
			name:      "identical definitions fingerprint equal across calls",
			wantEqual: true,
		},
		{
			name: "schema representation and key order do not matter",
			mutateB: func(tool *mcp.Tool) {
				tool.InputSchema = json.RawMessage(
					`{"required":["query"],"properties":{"limit":{"maximum":100,"type":"integer"},"query":{"type":"string"}},"type":"object"}`,
				)
			},
			wantEqual: true,
		},
		{
			name:      "name change is detected",
			mutateB:   func(tool *mcp.Tool) { tool.Name = "search-v2" },
			wantEqual: false,
		},
		{
			name:      "title change is detected",
			mutateB:   func(tool *mcp.Tool) { tool.Title = "Search (new)" },
			wantEqual: false,
		},
		{
			name:      "description change is detected",
			mutateB:   func(tool *mcp.Tool) { tool.Description = "Searches the index, faster." },
			wantEqual: false,
		},
		{
			name: "input schema change is detected",
			mutateB: func(tool *mcp.Tool) {
				tool.InputSchema = map[string]any{"type": "object"}
			},
			wantEqual: false,
		},
		{
			name: "output schema change is detected",
			mutateB: func(tool *mcp.Tool) {
				tool.OutputSchema = map[string]any{"type": "object"}
			},
			wantEqual: false,
		},
		{
			name: "annotations-only change is detected (readOnlyHint flip)",
			mutateB: func(tool *mcp.Tool) {
				tool.Annotations.ReadOnlyHint = false
			},
			wantEqual: false,
		},
		{
			name: "_meta-only change is detected",
			mutateB: func(tool *mcp.Tool) {
				tool.Meta = mcp.Meta{"rev": "2"}
			},
			wantEqual: false,
		},
		{
			name: "icons change is detected",
			mutateB: func(tool *mcp.Tool) {
				tool.Icons = []mcp.Icon{{Source: "https://example.com/icon-v2.png", MIMEType: "image/png"}}
			},
			wantEqual: false,
		},
		{
			name:      "nil and empty icons are wire-equivalent",
			mutateA:   func(tool *mcp.Tool) { tool.Icons = nil },
			mutateB:   func(tool *mcp.Tool) { tool.Icons = []mcp.Icon{} },
			wantEqual: true,
		},
		{
			name:      "nil and empty _meta are wire-equivalent",
			mutateA:   func(tool *mcp.Tool) { tool.Meta = nil },
			mutateB:   func(tool *mcp.Tool) { tool.Meta = mcp.Meta{} },
			wantEqual: true,
		},
		{
			name:      "absent annotations differ from an empty annotations object",
			mutateA:   func(tool *mcp.Tool) { tool.Annotations = nil },
			mutateB:   func(tool *mcp.Tool) { tool.Annotations = &mcp.ToolAnnotations{} },
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := fingerprintToolFixture()
			b := fingerprintToolFixture()
			if tt.mutateA != nil {
				tt.mutateA(a)
			}
			if tt.mutateB != nil {
				tt.mutateB(b)
			}

			fingerprintA, err := Fingerprint(a)
			require.NoError(t, err)
			fingerprintB, err := Fingerprint(b)
			require.NoError(t, err)

			if tt.wantEqual {
				assert.Equal(t, fingerprintA, fingerprintB, "expected wire-equivalent definitions to fingerprint equal")
			} else {
				assert.NotEqual(
					t,
					fingerprintA,
					fingerprintB,
					"expected the definition change to produce a different fingerprint",
				)
			}
		})
	}
}

func TestFingerprintErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tool    *mcp.Tool
		wantErr string
	}{
		{
			name:    "errors on a nil tool",
			tool:    nil,
			wantErr: "tool is nil",
		},
		{
			name:    "errors on an unmarshalable definition",
			tool:    unmarshalableToolFixture(),
			wantErr: `marshal tool "bad" wire definition`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Fingerprint(tt.tool)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, got, "expected no fingerprint alongside an error")
		})
	}
}

func TestFingerprintTools(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	good := fingerprintToolFixture()
	bad := unmarshalableToolFixture()

	got := fingerprintTools(t.Context(), logger, []*mcp.Tool{good, bad, nil})

	wantGood, err := Fingerprint(good)
	require.NoError(t, err)

	assert.Len(t, got, 2, "expected the nil tool to be skipped")
	assert.Equal(t, wantGood, got[good.Name], "expected each tool keyed by name to carry its Fingerprint")
	assert.True(t, strings.HasPrefix(got[bad.Name], "!"),
		"expected an unfingerprintable tool to be recorded under a marker that can never equal a hex fingerprint")

	again := fingerprintTools(t.Context(), logger, []*mcp.Tool{good, bad, nil})
	assert.Equal(t, got, again, "expected fingerprintTools to be deterministic")
}

// fingerprintToolFixture returns a fully populated tool definition: every
// wire field is set so each table case can flip exactly one of them.
func fingerprintToolFixture() *mcp.Tool {
	return &mcp.Tool{
		Name:        "search",
		Title:       "Search",
		Description: "Searches the index.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer", "maximum": 100},
			},
			"required": []any{"query"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"hits": map[string]any{"type": "array"},
			},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		Icons:       []mcp.Icon{{Source: "https://example.com/icon.png", MIMEType: "image/png"}},
		Meta:        mcp.Meta{"rev": "1"},
	}
}

// unmarshalableToolFixture returns a tool whose definition cannot be
// JSON-marshaled, which the upstream health gate is supposed to make
// unreachable.
func unmarshalableToolFixture() *mcp.Tool {
	return &mcp.Tool{
		Name:        "bad",
		InputSchema: map[string]any{"ch": make(chan int)},
	}
}
