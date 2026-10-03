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
	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() { agentstools.Register("centrum", "get_pricing", getPricingTool) }

func getPricingTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name:        "get_pricing",
		Description: "Get pricing for a date. Args: {date? (YYYY-MM-DD, default today)}. Returns open/close, day/evening price.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = agentstools.WibNow().Format("2006-01-02")
			}
			var open, close, dayStart, dayEnd string
			var priceDay, priceEvening float64
			var validFrom string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT valid_from::text, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening
					   FROM sports_pricing_rules WHERE tenant_id=$1 AND is_active AND valid_from <= $2::date ORDER BY valid_from DESC LIMIT 1`,
					tenantID, p.Date).Scan(&validFrom, &open, &close, &dayStart, &dayEnd, &priceDay, &priceEvening)
			})
			if err != nil {
				// Only a genuine "no active rule" is a no-pricing
				// outcome Ã¢â‚¬â€ a DB outage must surface as an error, not
				// a silent "no pricing" that the LLM would relay as fact.
				if errors.Is(err, pgx.ErrNoRows) {
					return "no pricing found for " + p.Date
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("pricing %s: %s-%s day %s-%s %.0f/%.0f (valid from %s)", p.Date, open, close, dayStart, dayEnd, priceDay, priceEvening, validFrom)
		},
	}
}
