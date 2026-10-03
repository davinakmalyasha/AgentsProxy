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

func init() { agentstools.Register("centrum", "record_staff_payment", recordStaffPaymentTool) }

func recordStaffPaymentTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "record_staff_payment", Mutating: true,
		Description: "Record a staff payment in the ledger (no real money moved). Args: {staff_name, amount, paid_at? (YYYY-MM-DD, default today), note?}.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				StaffName string  `json:"staff_name"`
				Amount    float64 `json:"amount"`
				PaidAt    string  `json:"paid_at"`
				Note      string  `json:"note"`
			}
			if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.StaffName) == "" {
				return "error: staff_name and amount required"
			}
			if p.Amount <= 0 {
				return "error: amount must be > 0"
			}
			if p.PaidAt == "" {
				p.PaidAt = agentstools.WibNow().Format("2006-01-02")
			}
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO agent_staff_payments (tenant_id, staff_name, amount, paid_at, note) VALUES ($1,$2,$3,$4::date,$5)`,
					tenantID, p.StaffName, p.Amount, p.PaidAt, p.Note)
				return err
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("recorded payment %s %.2f on %s", p.StaffName, p.Amount, p.PaidAt)
		},
	}
}
