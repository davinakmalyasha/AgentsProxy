// Role/capability tier tests.
//
// IsOwner() is TRUE for a client API token on purpose: a client site's
// backend writes business data through X-Client-Token and RLS pins the
// tenant. That model is the platform's API-integration story.
//
// IsStaffOwner() is false for it. A client token is one static secret,
// usually embedded in a site or an integration; letting it promote a
// staff account would turn a leaked
// integration credential into persistent human-level access.
//
// These tests pin both halves, because the failure mode of each is the
// opposite of the other's.
package clientctx

import "testing"

// rolePublic is the unauthenticated identity assigned by
// records.publicIdentity for whitelisted public reads. It is a string
// literal there rather than a declared constant, so it is declared here
// to keep the table below aligned.
const rolePublic = "public"

func TestIsOwner_AdmitsClientTokensForDataAccess(t *testing.T) {
	// Business-data tier: owner and client both pass. If this starts
	// failing, API integrations lose write access.
	cases := map[string]bool{
		RoleOwner:  true,
		RoleClient: true,
		RoleStaff:  false,
		rolePublic: false,
	}
	for role, want := range cases {
		id := &Identity{Role: role}
		if got := id.IsOwner(); got != want {
			t.Errorf("IsOwner(%s) = %v, want %v", role, got, want)
		}
	}
}

func TestIsStaffOwner_RejectsClientTokens(t *testing.T) {
	// Security tier: only a real owner session. This is the one that
	// must never widen.
	cases := map[string]bool{
		RoleOwner:  true,
		RoleClient: false,
		RoleStaff:  false,
		rolePublic: false,
	}
	for role, want := range cases {
		id := &Identity{Role: role}
		if got := id.IsStaffOwner(); got != want {
			t.Errorf("IsStaffOwner(%s) = %v, want %v", role, want, want)
		}
	}
}

func TestNilIdentity_IsNeitherOwner(t *testing.T) {
	var id *Identity
	if id.IsOwner() {
		t.Error("nil identity must not pass IsOwner")
	}
	if id.IsStaffOwner() {
		t.Error("nil identity must not pass IsStaffOwner")
	}
}

func TestUnknownRole_IsNeitherOwner(t *testing.T) {
	// A role string the code does not know about must fail closed, not
	// default to privileged.
	id := &Identity{Role: "superadmin"}
	if id.IsOwner() {
		t.Error("unknown role must not pass IsOwner")
	}
	if id.IsStaffOwner() {
		t.Error("unknown role must not pass IsStaffOwner")
	}
}

func TestEmptyRole_IsNeitherOwner(t *testing.T) {
	id := &Identity{}
	if id.IsOwner() || id.IsStaffOwner() {
		t.Error("empty role must fail closed on both tiers")
	}
}

// IsStaffOwner is strictly narrower than IsOwner: anything that passes
// the staff tier must also pass the data tier, and the reverse need
// not hold. If this ever fails, RequireStaffOwner has silently become
// as permissive as RequireOwner and the split is pointless.
func TestStaffOwnerIsStrictlyNarrowerThanOwner(t *testing.T) {
	roles := []string{RoleOwner, RoleClient, RoleStaff, rolePublic, "", "unknown"}
	for _, r := range roles {
		id := &Identity{Role: r}
		if id.IsStaffOwner() && !id.IsOwner() {
			t.Errorf("role %q passes IsStaffOwner but not IsOwner; the tiers are inconsistent", r)
		}
	}
	// And there must be at least one role that passes the data tier
	// but not the security tier, or the split is not doing anything.
	sawGap := false
	for _, r := range roles {
		id := &Identity{Role: r}
		if id.IsOwner() && !id.IsStaffOwner() {
			sawGap = true
		}
	}
	if !sawGap {
		t.Error("no role distinguishes the tiers; RequireStaffOwner adds nothing over RequireOwner")
	}
}
