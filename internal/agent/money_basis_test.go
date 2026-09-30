// DB-free tests for the shared money basis.
//
// The agent had three different answers for "how much money is there":
// query_income and query_net_profit summed booking prices (value
// committed), while query_range_summary and closing_report summed
// sports_payments (money received). The owner's Laporan screen is
// payments-based, so the bot and the screen could disagree about the same
// day while each tool was internally consistent.
//
// These tests pin the arithmetic that keeps the two figures distinct, so
// a future edit cannot quietly collapse one into the other.
package agent

import (
	"strings"
	"testing"
)

func TestReceivedAddsEveryMoneySource(t *testing.T) {
	b := basis{Cash: 100000, QRIS: 50000, Other: 25000, Kantin: 30000}
	// Cash + QRIS + any other method + canteen. The "Other" bucket exists
	// because the old query filtered only on cash/qris and would have
	// silently dropped a transfer as if it did not exist.
	if got := b.Received(); got != 205000 {
		t.Errorf("Received() = %v, want 205000", got)
	}
}

func TestReceivedIgnoresCommittedValue(t *testing.T) {
	// The whole point: 3 bookings worth 450.000 with nothing collected
	// is zero received, not 450.000.
	b := basis{Cash: 50000, Booked: 450000}
	if got := b.Received(); got != 50000 {
		t.Errorf("Received() = %v, want 50000 (Booked must not count as income)", got)
	}
}

func TestReceivedWithNoActivity(t *testing.T) {
	var b basis
	if got := b.Received(); got != 0 {
		t.Errorf("Received() on an empty basis = %v, want 0", got)
	}
	if got := b.describe(); !strings.Contains(got, "terima 0") {
		t.Errorf("describe() should lead with a zero received figure, got %q", got)
	}
}

func TestDescribeLabelsBothFigures(t *testing.T) {
	b := basis{Cash: 120000, QRIS: 80000, Kantin: 45000, Booked: 400000}
	got := b.describe()
	// Both words, so the model has no way to read one as the other.
	if !strings.Contains(got, "terima") || !strings.Contains(got, "teresan") {
		t.Errorf("describe() must label both figures, got %q", got)
	}
	// Indonesian labels, matching how the model is prompted to answer.
	if !strings.Contains(got, "terima 245000") {
		t.Errorf("describe() received total wrong, got %q", got)
	}
	if !strings.Contains(got, "teresan 400000") {
		t.Errorf("describe() committed total wrong, got %q", got)
	}
	// The cash/qris split is what the owner reconciles against the till.
	if !strings.Contains(got, "cash 120000") || !strings.Contains(got, "qris 80000") {
		t.Errorf("describe() lost the payment method split, got %q", got)
	}
	// Canteen is money in too, and must be inside the received total
	// rather than listed separately where it could be double-counted.
	if !strings.Contains(got, "kantin 45000") {
		t.Errorf("describe() lost the canteen figure, got %q", got)
	}
}

func TestDescribeNeverPrintsAnEmptyLabel(t *testing.T) {
	// A zero-valued basis is a normal state (new tenant) and must read
	// cleanly rather than as "cash 0, qris 0" noise the model has to
	// explain. It is still allowed here — the point is that it is
	// unambiguous, not that it is silent.
	got := basis{}.describe()
	if strings.Count(got, "terima") != 1 {
		t.Errorf("describe() should mention 'terima' exactly once, got %q", got)
	}
}
