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

func init() { agentstools.Register("centrum", "topup_member", topupMemberTool) }

func topupMemberTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "topup_member", Mutating: true,
		Description: "Add balance to a member. Args: {member_id, amount}. Returns new balance.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				MemberID string  `json:"member_id"`
				Amount   float64 `json:"amount"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.MemberID == "" {
				return "error: member_id and amount required"
			}
			if p.Amount <= 0 {
				return "error: amount must be > 0"
			}
			var bal float64
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`UPDATE sports_members SET balance = balance + $1, updated_at=now()
					  WHERE id=$2::uuid AND tenant_id=$3 RETURNING balance`,
					p.Amount, p.MemberID, tenantID).Scan(&bal)
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: member not found"
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("new balance: %.2f", bal)
		},
	}
}
