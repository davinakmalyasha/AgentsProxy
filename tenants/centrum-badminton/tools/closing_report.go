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
		Name:        "closing_report",
		Description: "Daily closing report (WhatsApp-ready). Args: {date? (YYYY-MM-DD)}. Revenue cash/qris, booking count, occupancy, unpaid list.",
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
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount) FILTER (WHERE method='cash'),0), COALESCE(SUM(amount) FILTER (WHERE method='qris'),0)
				   FROM sports_payments WHERE tenant_id=$1 AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date = $2::date`,
					tenantID, p.Date).Scan(&cash, &qris); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings WHERE tenant_id=$1 AND date=$2::date AND status NOT IN ('cancelled','no_show')`,
					tenantID, p.Date).Scan(&bookings); err != nil {
					return err
				}
				return tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings WHERE tenant_id=$1 AND date=$2::date AND payment_status != 'paid' AND status NOT IN ('cancelled','no_show')`,
					tenantID, p.Date).Scan(&unpaid)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("CLOSING REPORT %s\nRevenue: %.0f (cash %.0f, qris %.0f)\nBookings: %d\nUnpaid: %d",
				p.Date, cash+qris, cash, qris, bookings, unpaid)
		},
	}
}
