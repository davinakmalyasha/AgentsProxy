// Caps fail-closed tests: a Redis outage must deny (money guard), never
// allow unlimited LLM spend on the shared key. The nil-RDB bypass is
// test-harness only.
package agentstools

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// deadRDB returns a client that fails every command (nothing listens).
func deadRDB() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: time.Second})
}

func TestCostCapRedisErrorDenies(t *testing.T) {
	old := CapRDB
	CapRDB = deadRDB()
	defer func() { CapRDB = old }()
	defer CapRDB.Close()
	if CostCapAllowedForRole(context.Background(), "t", "owner") {
		t.Error("cost cap allowed on Redis error, want deny (fail-closed)")
	}
}

func TestRPDCapRedisErrorDenies(t *testing.T) {
	old := CapRDB
	CapRDB = deadRDB()
	defer func() { CapRDB = old }()
	defer CapRDB.Close()
	if RPDCapAllowedForUser(context.Background(), "t", "client", "u") {
		t.Error("rpd cap allowed on Redis error, want deny (fail-closed)")
	}
}

func TestCapsNilRDBBypassDocumented(t *testing.T) {
	old := CapRDB
	CapRDB = nil
	defer func() { CapRDB = old }()
	// Nil means "no Redis wired" (unit tests). Production always wires it.
	if !CostCapAllowedForRole(context.Background(), "t", "owner") {
		t.Error("nil RDB should bypass (test-only), got deny")
	}
	if !RPDCapAllowedForUser(context.Background(), "t", "client", "u") {
		t.Error("nil RDB should bypass (test-only), got deny")
	}
}
