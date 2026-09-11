package reloader

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxToolNameLength mirrors the SDK's tool-name length limit.
const maxToolNameLength = 128

// objectType is the JSON Schema type the downstream AddTool requires of
// every tool input and output schema.
const objectType = "object"

// ValidateTool reports why one tool definition is unsafe to register
// downstream.
//
// The checks mirror the conditions under which the downstream server's
// non-generic AddTool panics — input schema present, object-typed, and
// marshalable, with the optional output schema held to the same shape only
// when present — plus the SDK's tool-name rules, which AddTool itself only
// logs before registering anyway. Child tool definitions are untrusted
// input; a definition that passes ValidateTool can never reach the AddTool
// panic path.
//
// It is exported for the same reason as [Fingerprint]: the upstream
// adapter's health gate and the downstream adapter's Reconcile must apply
// the identical check.
func ValidateTool(tool *mcp.Tool) error {
	if tool == nil {
		return errors.New("tool is nil")
	}
	if err := validateToolName(tool.Name); err != nil {
		return fmt.Errorf("tool %q: %w", tool.Name, err)
	}
	if tool.InputSchema == nil {
		return fmt.Errorf("tool %q: missing input schema", tool.Name)
	}
	if err := validateObjectSchema(tool.InputSchema); err != nil {
		return fmt.Errorf("tool %q: input schema: %w", tool.Name, err)
	}
	if tool.OutputSchema != nil {
		if err := validateObjectSchema(tool.OutputSchema); err != nil {
			return fmt.Errorf("tool %q: output schema: %w", tool.Name, err)
		}
	}
	return nil
}

// ValidateTools validates every definition in tools and rejects duplicate
// names. The reconcile diff keys on name, so a duplicate is ambiguous even
// when both definitions are individually valid.
func ValidateTools(tools []*mcp.Tool) error {
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if err := ValidateTool(tool); err != nil {
			return err
		}
		if seen[tool.Name] {
			return fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true
	}
	return nil
}

// validateToolName mirrors the SDK's unexported tool-name validation:
// non-empty, at most maxToolNameLength bytes, charset [a-zA-Z0-9_.-]. The
// SDK only logs an invalid name, so the proxy enforces it here.
func validateToolName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	if len(name) > maxToolNameLength {
		return fmt.Errorf("name exceeds %d characters", maxToolNameLength)
	}
	for _, r := range name {
		if !validToolNameRune(r) {
			return fmt.Errorf("name contains invalid character %q", r)
		}
	}
	return nil
}

// validToolNameRune mirrors the SDK's tool-name charset.
func validToolNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' || r == '-' || r == '.'
}

// validateObjectSchema checks that schema marshals to a JSON object whose
// "type" is "object" — exactly what the downstream AddTool requires of both
// input and output schemas. Schemas received from a child arrive as
// map[string]any, so the remarshal is cheap.
func validateObjectSchema(schema any) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("not marshalable: %w", err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}
	if typ := object["type"]; typ != objectType {
		return fmt.Errorf(`type is %v, not "object"`, typ)
	}
	return nil
}
