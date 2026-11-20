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

func init() { agentstools.Register("centrum", "check_stock", checkStockTool) }

func checkStockTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "check_stock",
		Description: "Inventory levels with low-stock flags. Use to check before promising " +
			"items; to change levels use adjust_stock.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"product or item name, may be partial"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(args, &p)
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				sql := `SELECT name, quantity, unit_price, low_stock_threshold FROM sports_inventory WHERE tenant_id=$1`
				qargs := []any{tenantID}
				if p.Name != "" {
					sql += " AND name ILIKE $2"
					qargs = append(qargs, "%"+p.Name+"%")
				}
				sql += " ORDER BY name LIMIT 50"
				r, err := tx.Query(ctx, sql, qargs...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name string
					var qty int
					var price, thr float64
					if err := r.Scan(&name, &qty, &price, &thr); err != nil {
						return err
					}
					flag := ""
					if float64(qty) <= thr {
						flag = " (LOW)"
					}
					rows = append(rows, fmt.Sprintf("%s: %d%s @ %s", name, qty, flag, agentstools.Idr(price)))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no inventory"
			}
			return strings.Join(rows, "\n")
		},
	}
}
