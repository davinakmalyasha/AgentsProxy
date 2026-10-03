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

func init() { agentstools.Register("centrum", "query_attendance", queryAttendanceTool) }

func queryAttendanceTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name:        "query_attendance",
		Description: "Count people who booked. Args: {date? (YYYY-MM-DD)}. Defaults to today. Returns count.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = agentstools.WibNow().Format("2006-01-02")
			}
			var n int
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings
					  WHERE tenant_id=$1 AND date=$2::date AND status IN ('confirmed','checked_in')`,
					tenantID, p.Date).Scan(&n)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("%d people booked on %s", n, p.Date)
		},
	}
}
