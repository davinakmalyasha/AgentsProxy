// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
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
			var b agentstools.Basis
			var expenses float64
			var bookings int
			var bestDay string
			var bestDayAmt float64
			peak := map[int]int{}
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				var err error
				if b, err = agentstools.LoadBasis(ctx, tx, tenantID, from, to); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount),0) FROM sports_expenses
					  WHERE tenant_id=$1 AND spent_at >= $2::date AND spent_at < $3::date`,
					tenantID, from, to).Scan(&expenses); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings
					  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
					    AND status NOT IN ('cancelled','no_show')`,
					tenantID, from, to).Scan(&bookings); err != nil {
					return err
				}
				// Best day is the day money actually came in, matching the
				// rest of the tool. Ordering by booking value instead
				// would crown a day full of unpaid bookings.
				if err := tx.QueryRow(ctx,
					`SELECT (paid_at AT TIME ZONE 'Asia/Jakarta')::date::text, SUM(amount)
					   FROM sports_payments
					  WHERE tenant_id=$1
					    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
					    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3::date
					  GROUP BY 1 ORDER BY 2 DESC LIMIT 1`,
					tenantID, from, to).Scan(&bestDay, &bestDayAmt); err != nil &&
					!errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				// Peak hours come from when bookings start, which is the
				// question a court owner actually asks.
				rows, err := tx.Query(ctx,
					`SELECT EXTRACT(HOUR FROM start_time)::int, COUNT(*)
					   FROM sports_bookings
					  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
					    AND status NOT IN ('cancelled','no_show')
					  GROUP BY 1 ORDER BY 2 DESC LIMIT 3`,
					tenantID, from, to)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var h, n int
					if err := rows.Scan(&h, &n); err != nil {
						return err
					}
					peak[h] = n
				}
				return rows.Err()
			})
			if err != nil {
				return "error: " + err.Error()
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
			if bestDay != "" {
				best = fmt.Sprintf("%s (%.0f)", bestDay, bestDayAmt)
			}
			return fmt.Sprintf("range %s to %s: %s, expenses %.0f, net %.0f, payments %d, bookings %d, best day %s%s",
				from, to, b.Describe(), expenses, b.Received()-expenses, b.Count, bookings, best, peakTxt)
		},
	}
}
