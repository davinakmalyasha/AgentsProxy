// WIB day-boundary fixtures (Phase B4). These pin the semantics every
// business-day bucket depends on: WIB is UTC+7 with no DST, and the venue
// date flips at 00:00 WIB (17:00 UTC of the previous day) — not at server
// midnight. If these fail, laporan/pembayaran/riwayat totals move days.
package agentstools

import (
	"strings"
	"testing"
	"time"
)

// TestWibLocIsAlwaysPlusSeven: no DST, January and July identical.
func TestWibLocIsAlwaysPlusSeven(t *testing.T) {
	for _, instant := range []time.Time{
		time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	} {
		name, offset := instant.In(WibLoc()).Zone()
		if name != "WIB" || offset != 7*3600 {
			t.Errorf("%v in WIB = %s %+d, want WIB +25200", instant, name, offset)
		}
	}
}

// TestWibDayBoundary: the venue date flips exactly at 00:00 WIB.
func TestWibDayBoundary(t *testing.T) {
	cases := []struct {
		utc  time.Time
		want string
	}{
		// 23:59:59 WIB Dec 31 → still Dec 31.
		{time.Date(2025, 12, 31, 16, 59, 59, 0, time.UTC), "2025-12-31"},
		// 00:00:00 WIB Jan 1 → Jan 1.
		{time.Date(2025, 12, 31, 17, 0, 0, 0, time.UTC), "2026-01-01"},
		// 06:59:59 WIB (23:59:59 UTC previous day) → still previous UTC day
		// in UTC terms, but the WIB date is what the venue bills.
		{time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC), "2026-07-01"},
		// 12:00 UTC = 19:00 WIB, same calendar day both sides.
		{time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), "2026-09-05"},
	}
	for _, c := range cases {
		if got := c.utc.In(WibLoc()).Format("2006-01-02"); got != c.want {
			t.Errorf("%v WIB date = %s, want %s", c.utc, got, c.want)
		}
	}
}

// TestCapKeysEmbedWibDate: cap buckets reset at venue midnight. (Keys embed
// today's date; asserting the date part equals the WIB date pins the policy
// without freezing time.)
func TestCapKeysEmbedWibDate(t *testing.T) {
	today := WibNow().Format("2006-01-02")
	for name, key := range map[string]string{
		"cap":     CapKey("t"),
		"capRole": CapKeyForRole("t", "owner"),
		"rpd":     RPDKeyForUser("t", "client", "u"),
	} {
		if !strings.HasSuffix(key, ":"+today) {
			t.Errorf("%s key = %q, want suffix %q", name, key, ":"+today)
		}
	}
}

// TestDefaultMonthRangeIsWibMonth: month helpers follow the venue calendar.
func TestDefaultMonthRangeIsWibMonth(t *testing.T) {
	from, to := DefaultMonthRange("", "")
	wantFrom := WibNow().Format("2006-01") + "-01"
	if from != wantFrom {
		t.Errorf("from = %s, want %s", from, wantFrom)
	}
	if !strings.HasPrefix(to, WibNow().AddDate(0, 1, 0).Format("2006-01")) {
		t.Errorf("to = %s, want next WIB month", to)
	}
}
