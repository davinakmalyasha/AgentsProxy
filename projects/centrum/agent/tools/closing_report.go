// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"fmt"
	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() { agentstools.Register("centrum", "closing_report", closingReportTool) }

func closingReportTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "closing_report",
		Description: "One day's operations summary: revenue, bookings, unpaid count. " +
			"Use for daily closing; for arbitrary periods use query_income.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to close, defaults to today"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = agentstools.WibNow().Format("2006-01-02")
			}
			var cash, qris float64
			var bookings, unpaid int
			// One round trip, not three: payments aggregates plus both
			// booking counts in a single row. Same predicates, same
			// semantics — 3 RTTs halved to 1 on every closing call.
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method='cash'),0),
					        COALESCE(SUM(p.amount) FILTER (WHERE p.method='qris'),0),
					        (SELECT COUNT(*) FROM sports_bookings b WHERE b.tenant_id=$1 AND b.date=$2::date AND b.status NOT IN ('cancelled','no_show')),
					        (SELECT COUNT(*) FROM sports_bookings b WHERE b.tenant_id=$1 AND b.date=$2::date AND b.payment_status != 'paid' AND b.status NOT IN ('cancelled','no_show'))
					   FROM sports_payments p WHERE p.tenant_id=$1 AND p.deleted_at IS NULL AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date = $2::date`,
					tenantID, p.Date).Scan(&cash, &qris, &bookings, &unpaid)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("CLOSING REPORT %s\nRevenue: %.0f (cash %.0f, qris %.0f)\nBookings: %d\nUnpaid: %d",
				p.Date, cash+qris, cash, qris, bookings, unpaid)
		},
	}
}
