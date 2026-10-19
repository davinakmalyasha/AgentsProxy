package agentstools

import (
	"testing"
	"time"
)

func TestShouldDeferReport(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		days int
		want bool
	}{
		{1, false},
		{ReportSyncMaxDays, false},
		{ReportSyncMaxDays + 1, true},
		{370, true},
	}
	for _, c := range cases {
		to := from.AddDate(0, 0, c.days)
		if got := ShouldDeferReport(from, to); got != c.want {
			t.Errorf("%d days: ShouldDeferReport=%v want %v", c.days, got, c.want)
		}
	}
}
