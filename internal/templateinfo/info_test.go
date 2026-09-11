package templateinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvPrefix(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "TEMPLATE_MCP", EnvPrefix())
}
