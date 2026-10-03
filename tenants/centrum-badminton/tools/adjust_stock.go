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

func init() { agentstools.Register("centrum", "adjust_stock", adjustStockTool) }

func adjustStockTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "adjust_stock", Mutating: true,
		Description: "Adjust stock by delta (positive=restock, negative=sale). Args: {item_id, delta}. Returns new quantity.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				ItemID string `json:"item_id"`
				Delta  int    `json:"delta"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.ItemID == "" {
				return "error: item_id and delta required"
			}
			var newQty int
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`UPDATE sports_inventory SET quantity = quantity + $1, updated_at=now()
					  WHERE id=$2::uuid AND tenant_id=$3 AND quantity + $1 >= 0
					  RETURNING quantity`,
					p.Delta, p.ItemID, tenantID).Scan(&newQty)
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: insufficient stock or item not found"
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("new quantity: %d", newQty)
		},
	}
}
