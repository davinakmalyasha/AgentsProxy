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

func init() { agentstools.Register("list_staff_payments", listStaffPaymentsTool) }

func listStaffPaymentsTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name:        "list_staff_payments",
		Description: "List staff payment ledger. Args: {staff_name?, from? (YYYY-MM-DD), to? (YYYY-MM-DD)}.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				StaffName string `json:"staff_name"`
				From      string `json:"from"`
				To        string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				sql := `SELECT staff_name, amount, paid_at::text, note FROM agent_staff_payments WHERE tenant_id=$1`
				qargs := []any{tenantID}
				idx := 2
				if p.StaffName != "" {
					sql += fmt.Sprintf(" AND staff_name ILIKE $%d", idx)
					qargs = append(qargs, "%"+p.StaffName+"%")
					idx++
				}
				if p.From != "" {
					sql += fmt.Sprintf(" AND paid_at >= $%d::date", idx)
					qargs = append(qargs, p.From)
					idx++
				}
				if p.To != "" {
					sql += fmt.Sprintf(" AND paid_at <= $%d::date", idx)
					qargs = append(qargs, p.To)
					idx++
				}
				sql += " ORDER BY paid_at DESC LIMIT 20"
				r, err := tx.Query(ctx, sql, qargs...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name, paidAt, note string
					var amt float64
					if err := r.Scan(&name, &amt, &paidAt, &note); err != nil {
						return err
					}
					rows = append(rows, fmt.Sprintf("%s %s %.2f %s", paidAt, name, amt, note))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no staff payments found"
			}
			return strings.Join(rows, "\n")
		},
	}
}
