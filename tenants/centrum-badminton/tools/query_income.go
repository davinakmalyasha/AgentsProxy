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
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

func init() { agentstools.Register("centrum", "query_income", queryIncomeTool) }

func queryIncomeTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "query_income",
		Description: "Money actually received in a period (payments plus canteen), with the " +
			"committed booking value alongside. Use for 'how much did we take' questions. " +
			"For profit after expenses use query_net_profit; for one operating day use closing_report.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive, defaults to this month"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE, defaults to next month"},"month":{"type":"string","description":"shorthand YYYY-MM for a whole month, ignored when from/to are given"}}}`),
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
			// One round trip, no transaction (was BEGIN + LoadBasis×3 +
			// COMMIT = 5 RTs). See LoadRangeSummary for the scoping note.
			s, err := agentstools.LoadRangeSummary(ctx, pool, tenantID, from, to)
			if err != nil {
				return "error: " + err.Error()
			}
			b := s.Basis()
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
