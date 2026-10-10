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
		// One round trip, no transaction (was BEGIN + LoadBasis×3 +
		// expenses + COMMIT = 6 RTs). private.range_summary runs as
		// money_reader; tenant scoping is its p_tenant predicate.
		s, err := agentstools.LoadRangeSummary(ctx, pool, tenantID, from, to)
		if err != nil {
			return "error: " + err.Error()
		}
		b := s.Basis()
		expenses := s.Expenses
		// Revenue is the money in, not the money promised. Summing
		// bookings here made a venue look profitable while nothing
		// had been collected.
		net := b.Received() - expenses
		return fmt.Sprintf("net profit %s (%s to %s): %s, expenses %s",
			agentstools.Idr(net), from, to, b.Describe(), agentstools.Idr(expenses))
		},
	}
}
