// DB-free tests for the booking state machine.
//
// These run without TEST_DATABASE_URL/TEST_REDIS_URL on purpose. The integration
// tests in feature_test.go are where the SQL is proven, but they skip
// whenever a managed pooler is in play (the harness has to CREATE ROLE
// app, which the hosted pooler refuses), so the rules below were the
// part of the cancel fix with no coverage at all in a normal run.
package bookings

import (
	"strings"
	"testing"
)

func TestCancellableFromMatchesTheTransitionTable(t *testing.T) {
	// cancellableFrom is derived from validTransitions, so if the table
	// gains a cancel edge the derived set follows automatically. This
	// test pins the resulting policy, which is the part that matters.
	for _, s := range []string{"pending", "confirmed"} {
		if !cancellableFrom[s] {
			t.Errorf("%s should be cancellable", s)
		}
	}
	// checked_in and no_show describe a slot that was really used —
	// cancelling them would erase a court event and re-open an occupied
	// slot. completed and cancelled are terminal.
	for _, s := range []string{"checked_in", "no_show", "completed", "cancelled"} {
		if cancellableFrom[s] {
			t.Errorf("%s must NOT be cancellable", s)
		}
	}
}

func TestCancellableFromHasNoExtraStates(t *testing.T) {
	// A state that does not exist must never appear in the DELETE
	// predicate — it would only ever be dead SQL, but it would also mean
	// the two tables had drifted.
	if len(cancellableFrom) != 2 {
		t.Errorf("cancellableFrom has %d entries, want 2: %v", len(cancellableFrom), cancellableFrom)
	}
	for s := range cancellableFrom {
		if !validBookingStatuses[s] {
			t.Errorf("cancellableFrom contains unknown status %q", s)
		}
	}
}

func TestCancellableFromSQLIsAQuotedSortedList(t *testing.T) {
	got := cancellableFromSQL
	if got != "'confirmed','pending'" {
		t.Errorf("cancellableFromSQL = %s, want 'confirmed','pending'", got)
	}
	// Quoted, comma-separated, no stray characters that could escape the
	// literal back into SQL.
	if strings.ContainsAny(got, ";-\n") {
		t.Errorf("cancellableFromSQL contains SQL metacharacters: %q", got)
	}
	// Every state in the map must be represented exactly once.
	for s := range cancellableFrom {
		if !strings.Contains(got, "'"+s+"'") {
			t.Errorf("cancellableFromSQL missing %q: %s", s, got)
		}
	}
}

func TestEveryCancellableStateCanReachCancelled(t *testing.T) {
	// The two views of the same rule must not disagree: anything DELETE
	// accepts has to be reachable through the PATCH transition table too.
	for s := range cancellableFrom {
		if len(validTransitions[s]) == 0 {
			t.Errorf("%s is cancellable per DELETE but has no transitions in the table", s)
		}
		found := false
		for _, to := range validTransitions[s] {
			if to == "cancelled" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is cancellable per DELETE but not per validTransitions", s)
		}
	}
}

func TestTerminalStatesHaveNoOutgoingTransitions(t *testing.T) {
	// A state with no entry in validTransitions is terminal: PATCH
	// refuses every transition out of it. The DELETE gate must agree, or
	// DELETE becomes the back door that reopens a finished booking.
	for _, s := range []string{"completed", "cancelled"} {
		if len(validTransitions[s]) != 0 {
			t.Errorf("%s should be terminal, has %v", s, validTransitions[s])
		}
		if cancellableFrom[s] {
			t.Errorf("%s is terminal but still cancellable", s)
		}
	}
}

func TestBuildUpdateSQLAddsTheStatusGuard(t *testing.T) {
	sets := []string{"status = $1", "updated_at = now()"}

	guarded, extra := buildUpdateSQL(sets, 3, true)
	if extra != 1 {
		t.Errorf("extraArgs = %d, want 1", extra)
	}
	// The guard must be in the WHERE clause, and the id/tenant predicate
	// must still be there — the point is to narrow the match, not replace
	// it.
	for _, want := range []string{"$3::uuid", "$4", "AND status = $5", "updated_at = now()"} {
		if !strings.Contains(guarded, want) {
			t.Errorf("guarded SQL missing %q:\n%s", want, guarded)
		}
	}
	if strings.Count(guarded, "WHERE") != 1 {
		t.Errorf("expected exactly one WHERE clause:\n%s", guarded)
	}
	// The guard must come after the id/tenant match, otherwise it reads
	// as a SET column.
	if strings.Index(guarded, "AND status") < strings.Index(guarded, "WHERE") {
		t.Errorf("status guard must follow WHERE:\n%s", guarded)
	}
}

func TestBuildUpdateSQLOmitsTheGuardWhenNotCheckingStatus(t *testing.T) {
	sets := []string{"notes = $1", "updated_at = now()"}

	plain, extra := buildUpdateSQL(sets, 3, false)
	if extra != 0 {
		t.Errorf("extraArgs = %d, want 0 (nothing to append)", extra)
	}
	// A price or notes edit carries no expected state, so adding a status
	// predicate here would be a lie: it would make the update fail
	// whenever a concurrent status change happened, even though the
	// caller never validated a transition.
	if strings.Contains(plain, "status =") {
		t.Errorf("unguarded SQL should not mention status:\n%s", plain)
	}
	if !strings.Contains(plain, "$3::uuid") || !strings.Contains(plain, "$4") {
		t.Errorf("unguarded SQL lost the id/tenant predicate:\n%s", plain)
	}
}

func TestBuildUpdateSQLKeepsTheGuardOffTheSetList(t *testing.T) {
	// A status column IS in `sets` for a real transition (that's the
	// write). The guard must be a WHERE term, so the statement has to
	// reference status exactly twice: once in SET, once in WHERE.
	guarded, _ := buildUpdateSQL([]string{"status = $1", "updated_at = now()"}, 3, true)
	if n := strings.Count(guarded, "status"); n != 2 {
		t.Errorf("status appears %d times, want 2 (one SET, one WHERE):\n%s", n, guarded)
	}
}
