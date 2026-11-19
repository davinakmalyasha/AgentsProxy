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
	"strings"
)

func init() { agentstools.Register("centrum", "query_kantin_summary", queryKantinSummaryTool) }

func queryKantinSummaryTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "query_kantin_summary",
		Description: "Canteen P&L for one day with top products. Use for kantin-specific " +
			"questions; kantin totals also appear inside query_income.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to summarize, defaults to today"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = agentstools.WibNow().Format("2006-01-02")
			}
			var revenue, cogs float64
			var items int
			var top []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(i.unit_price*i.qty),0), COALESCE(SUM(i.unit_cost*i.qty),0), COALESCE(SUM(i.qty),0)
					   FROM sports_kantin_sale_items i
					   JOIN sports_kantin_sales s ON s.id = i.sale_id
					  WHERE i.tenant_id=$1 AND s.voided_at IS NULL
					    AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date = $2::date`,
					tenantID, p.Date).Scan(&revenue, &cogs, &items); err != nil {
					return err
				}
				r, err := tx.Query(ctx,
					`SELECT i.product_name, SUM(i.qty) FROM sports_kantin_sale_items i
					   JOIN sports_kantin_sales s ON s.id = i.sale_id
					  WHERE i.tenant_id=$1 AND s.voided_at IS NULL
					    AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date = $2::date
					  GROUP BY i.product_name ORDER BY 2 DESC LIMIT 5`,
					tenantID, p.Date)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name string
					var qty int64
					if err := r.Scan(&name, &qty); err != nil {
						return err
					}
					top = append(top, fmt.Sprintf("%s x%d", name, qty))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("kantin %s: omzet %.0f, modal %.0f, laba %.0f, items %d, top: %s",
				p.Date, revenue, cogs, revenue-cogs, items, strings.Join(top, ", "))
		},
	}
}

// wibNow returns now in WIB (UTC+7, no DST). FixedZone avoids tzdata
