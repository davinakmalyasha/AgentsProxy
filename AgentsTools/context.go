package agentstools

import "time"

// WibNow returns now in WIB (UTC+7, no DST). FixedZone avoids tzdata
// dependency on Windows containers.
//
// TIME POLICY (single source of truth):
//   - Business-day buckets (caps keys, availability default date, expense
//     spent_at default, series "future", report months, prompt date) use
//     WIB via this function. Server-local midnight would bill the wrong
//     day for seven hours a day on a UTC host.
//   - Technical buckets stay UTC by explicit decision, not by accident:
//     quota month + resetAt wire format (quota.go), usage month
//     (usage/store.go), rate-limit minute bucket (ratelimit.go).
//     Migrating those to WIB needs double-write + reconciliation, because
//     quota months and report months would otherwise disagree on the
//     month boundary.
func WibNow() time.Time {
	return time.Now().In(WibLoc())
}

// WibLoc is the WIB location for ParseInLocation.
func WibLoc() *time.Location {
	return time.FixedZone("WIB", 7*3600)
}

// DefaultMonthRange returns (firstOfMonth, firstOfNextMonth) unless
// both bounds are given.
func DefaultMonthRange(from, to string) (string, string) {
	if from != "" && to != "" {
		return from, to
	}
	now := WibNow()
	first := now.Format("2006-01") + "-01"
	next := now.AddDate(0, 1, 0).Format("2006-01-02")
	return first, next
}
