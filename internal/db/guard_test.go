package db

import (
	"fmt"
	"os"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/testsafety"
)

// TestMain refuses a remote test target before any migration runs. The
// suite applies DDL and several tests delete rows; a stale shell export must
// not be able to point it at staging or production.
func TestMain(m *testing.M) {
	if err := testsafety.CheckLocal(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "refusing DB test run:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
