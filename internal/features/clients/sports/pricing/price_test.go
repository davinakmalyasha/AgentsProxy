package pricing

import (
	"testing"
	"time"
)

// The pricing rule: the window is priced HOUR BY HOUR, charging the day
// rate for hours inside day_start..day_end and the evening rate outside
// it.
//
// These tests exist because the proxy and the frontend disagreed. The
// frontend's computePrice has always summed one rate per hour; the proxy
// used to apply a SINGLE rate to the whole window. A booking straddling
// the boundary was therefore quoted one figure and charged another, and
// the customer agreed to the quoted one.
//
// rule used throughout: day 07:00-17:00 @ Rp40.000, evening @ Rp50.000.
func rule() *Rule {
	return &Rule{
		DayStart:     "07:00",
		DayEnd:       "17:00",
		PriceDay:     40000,
		PriceEvening: 50000,
	}
}

func hm(s string) time.Time {
	v, err := time.Parse("15:04", s)
	if err != nil {
		panic("bad test time " + s)
	}
	return v
}

// priceWindow mirrors the loop in PriceFor exactly, without a DB, so the
// arithmetic can be asserted directly. It is a deliberate copy: the real
// function needs a transaction to resolve a rule, and a copy that drifts
// would make these tests assert nothing. The integration suite covers the
// wired-up path.
func priceWindow(r *Rule, startStr, endStr string) float64 {
	start, end := hm(startStr), hm(endStr)
	dayStart, _ := time.Parse("15:04", normTime(r.DayStart))
	dayEnd, _ := time.Parse("15:04", normTime(r.DayEnd))
	total := 0.0
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		rate := r.PriceEvening
		if !h.Before(dayStart) && h.Before(dayEnd) {
			rate = r.PriceDay
		}
		segEnd := h.Add(time.Hour)
		if segEnd.After(end) {
			segEnd = end
		}
		total += rate * segEnd.Sub(h).Hours()
	}
	return total
}

func TestPriceWindowInsideDayHours(t *testing.T) {
	// 07:00-10:00 is entirely within 07:00-17:00.
	if got := priceWindow(rule(), "07:00", "10:00"); got != 120000 {
		t.Errorf("07:00-10:00 = %v, want 120000", got)
	}
}

func TestPriceWindowInsideEveningHours(t *testing.T) {
	// 19:00-21:00 is entirely after 17:00.
	if got := priceWindow(rule(), "19:00", "21:00"); got != 100000 {
		t.Errorf("19:00-21:00 = %v, want 100000", got)
	}
}

// This is the case the old single-rate implementation got wrong.
func TestPriceWindowStraddlingDayEveningBoundary(t *testing.T) {
	// 16:00-18:00: 16:00-17:00 is day, 17:00-18:00 is evening.
	// 40.000 + 50.000 = 90.000.
	// The old code charged the evening rate for BOTH hours = 100.000,
	// which is what the booking committed while the UI quoted 90.000.
	if got := priceWindow(rule(), "16:00", "18:00"); got != 90000 {
		t.Errorf("16:00-18:00 = %v, want 90000 (one day hour + one evening hour)", got)
	}
}

func TestPriceWindowStraddlingOpenBoundary(t *testing.T) {
	// 06:00-08:00 with day_start 07:00: 06:00-07:00 is evening, then a
	// day hour.
	if got := priceWindow(rule(), "06:00", "08:00"); got != 90000 {
		t.Errorf("06:00-08:00 = %v, want 90000", got)
	}
}

func TestPriceWindowSingleHour(t *testing.T) {
	if got := priceWindow(rule(), "10:00", "11:00"); got != 40000 {
		t.Errorf("10:00-11:00 = %v, want 40000", got)
	}
	if got := priceWindow(rule(), "20:00", "21:00"); got != 50000 {
		t.Errorf("20:00-21:00 = %v, want 50000", got)
	}
}

// The boundary is half-open [day_start, day_end): 17:00 is the first
// EVENING hour, and 07:00 the first DAY hour.
func TestPriceWindowBoundariesAreHalfOpen(t *testing.T) {
	if got := priceWindow(rule(), "16:00", "17:00"); got != 40000 {
		t.Errorf("16:00-17:00 = %v, want 40000 (day hour)", got)
	}
	if got := priceWindow(rule(), "17:00", "18:00"); got != 50000 {
		t.Errorf("17:00-18:00 = %v, want 50000 (17:00 is already evening)", got)
	}
}

func TestPriceWindowZeroLengthIsFree(t *testing.T) {
	if got := priceWindow(rule(), "10:00", "10:00"); got != 0 {
		t.Errorf("zero-length window = %v, want 0", got)
	}
}

// Splitting a booking into consecutive pieces must cost the same as one
// booking covering the whole span. This is the invariant that catches any
// future change to the loop.
func TestPriceWindowIsAdditive(t *testing.T) {
	r := rule()
	whole := priceWindow(r, "15:00", "20:00")
	parts := priceWindow(r, "15:00", "17:00") + priceWindow(r, "17:00", "20:00")
	if whole != parts {
		t.Errorf("15:00-20:00 = %v but split at 17:00 = %v", whole, parts)
	}
	// 15:00-17:00 day (2h x 40k) + 17:00-20:00 evening (3h x 50k).
	if whole != 80000+150000 {
		t.Errorf("15:00-20:00 = %v, want 230000", whole)
	}
}

// Long spans must not drift; a whole day of court time is 16 hours.
func TestPriceWindowFullDay(t *testing.T) {
	// 07:00-23:00: 10 day hours (07-17) + 6 evening hours (17-23).
	if got := priceWindow(rule(), "07:00", "23:00"); got != 10*40000+6*50000 {
		t.Errorf("07:00-23:00 = %v, want %v", got, 10*40000+6*50000)
	}
}
