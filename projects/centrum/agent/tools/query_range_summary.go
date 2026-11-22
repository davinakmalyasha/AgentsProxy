// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
)

func init() { agentstools.Register("centrum", "query_range_summary", queryRangeSummaryTool) }

func queryRangeSummaryTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "query_range_summary",
		Description: "Full-period dashboard: revenue split, expenses, net, counts, best day, " +
			"busiest hours. Use when the owner asks for the whole picture; for one number " +
			"use query_income or query_net_profit.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := agentstools.DefaultMonthRange(p.From, p.To)
			// One round trip inside the tenant transaction: the project function
			// runs all seven aggregates server-side. Output formatting is unchanged.
			s, err := loadRangeSummary(ctx, pool, tenantID, from, to)
			if err != nil {
				return "error: " + err.Error()
			}
			b := s.basis()
			peak := map[int]int{}
			for _, hc := range s.Peak {
				peak[hc[0]] = hc[1]
			}
			var peakTxt string
			if len(peak) > 0 {
				hours := make([]string, 0, len(peak))
				for h := 0; h < 24; h++ {
					if n, ok := peak[h]; ok {
						hours = append(hours, fmt.Sprintf("%02d:00 x%d", h, n))
					}
				}
				peakTxt = ", peak " + strings.Join(hours, ", ")
			}
			best := "n/a"
			if s.BestDay != nil {
				best = fmt.Sprintf("%s (%.0f)", *s.BestDay, s.BestDayAmt)
			}
			return fmt.Sprintf("range %s to %s: %s, expenses %.0f, net %.0f, payments %d, bookings %d, best day %s%s",
				from, to, b.describe(), s.Expenses, b.received()-s.Expenses, b.Count, s.Bookings, best, peakTxt)
		},
	}
}
