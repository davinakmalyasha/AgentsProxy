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
	"time"
)

func init() { agentstools.Register("query_income", queryIncomeTool) }

func queryIncomeTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "query_income",
		Description: "Income for a period: money actually received from payments plus canteen, " +
			"with the committed booking value reported alongside. " +
			"Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD), month? (YYYY-MM)}. Defaults to this month.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From  string `json:"from"`
				To    string `json:"to"`
				Month string `json:"month"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := p.From, p.To
			if from == "" && p.Month != "" {
				from = p.Month + "-01"
				t, _ := time.Parse("2006-01-02", from)
				to = t.AddDate(0, 1, 0).Format("2006-01-02")
			}
			if from == "" {
				now := agentstools.WibNow()
				from = now.Format("2006-01") + "-01"
				to = now.AddDate(0, 1, 0).Format("2006-01-02")
			}
			var b agentstools.Basis
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				var err error
				b, err = agentstools.LoadBasis(ctx, tx, tenantID, from, to)
				return err
			})
			if err != nil {
				return "error: " + err.Error()
			}
			// "Income" is money in the drawer, not the value of slots that
			// were reserved. This used to sum booking prices, so a fully
			// unpaid month reported income while the Laporan screen the
			// owner reads showed zero. Both figures are returned now, with
			// the received one first.
			return fmt.Sprintf("income %s (%s to %s), %d payments",
				b.Describe(), from, to, b.Count)
		},
	}
}
