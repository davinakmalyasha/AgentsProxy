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

func init() { agentstools.Register("centrum", "query_net_profit", queryNetProfitTool) }

func queryNetProfitTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "query_net_profit",
		Description: "Profit actually received over a range: income minus expenses. " +
			"Use for 'laba/untung' questions. For received money without expenses use query_income.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := agentstools.DefaultMonthRange(p.From, p.To)
			var b agentstools.Basis
			var expenses float64
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				var err error
				if b, err = agentstools.LoadBasis(ctx, tx, tenantID, from, to); err != nil {
					return err
				}
				return tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount),0) FROM sports_expenses
					  WHERE tenant_id=$1 AND deleted_at IS NULL AND spent_at >= $2::date AND spent_at < $3::date`,
					tenantID, from, to).Scan(&expenses)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			// Revenue is the money in, not the money promised. Summing
			// bookings here made a venue look profitable while nothing
			// had been collected.
			net := b.Received() - expenses
			return fmt.Sprintf("net profit %.0f (%s to %s): %s, expenses %.0f",
				net, from, to, b.Describe(), expenses)
		},
	}
}
