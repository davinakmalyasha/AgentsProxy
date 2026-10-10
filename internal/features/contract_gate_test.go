package features_test

import (
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/features"

	// Named imports (not blank): the registry is process-global and
	// other tests ResetForTest it, so this test re-registers the real
	// backends itself, exactly like validators_test.reregister does.
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/availability"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/bookings"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/courtblocks"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/customers"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/expenses"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/facilities"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/inventory"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/kantin"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/members"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/payments"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/pricing"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/series"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/users"
)

// contractGated lists the custom features whose writes pass
// records.CheckContractTx (tenant_schema_contracts, migration 049).
// Every other custom feature is an explicit exemption below — not an
// oversight. Adding a feature (or gating one) must update this map,
// which is the whole point: a new write path can never slip in
// ungated without someone declaring it.
var contractGated = map[string]bool{
	"bookings":     true,
	"pricing":      true,
	"availability": false,
	"courtblocks":  false,
	"customers":    false,
	"expenses":     false,
	"facilities":   false,
	"inventory":    false,
	"kantin":       false,
	"members":      false,
	"payments":     false,
	"series":       false,
	"users":        false,
}

func TestContractGateCoverage(t *testing.T) {
	features.ResetForTest()
	availability.Register()
	bookings.Register()
	courtblocks.Register()
	customers.Register()
	expenses.Register()
	facilities.Register()
	inventory.Register()
	kantin.Register()
	members.Register()
	payments.Register()
	pricing.Register()
	series.Register()
	users.Register()
	seen := map[string]bool{}
	for _, f := range features.All() {
		if f.Routes == nil {
			continue // adapter-served shared features carry no schema
		}
		seen[f.Name] = true
		gated, declared := contractGated[f.Name]
		if !declared {
			t.Errorf("custom feature %q is not declared in contractGated: gate it or exempt it explicitly", f.Name)
			continue
		}
		if gated {
			t.Logf("feature %q: contract-gated", f.Name)
		}
	}
	for name := range contractGated {
		if !seen[name] {
			t.Errorf("contractGated lists %q, which is not a registered custom feature (renamed?)", name)
		}
	}
}
