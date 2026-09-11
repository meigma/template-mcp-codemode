package mcpserver

import (
	"os"
	"testing"

	"github.com/meigma/codemode"
)

func TestMain(m *testing.M) {
	codemode.ServeWorkerAndExit()
	os.Exit(m.Run())
}
