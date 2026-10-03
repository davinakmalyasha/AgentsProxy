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

func init() { agentstools.Register("centrum", "list_bookings", listBookingsTool) }

func listBookingsTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name:        "list_bookings",
		Description: "List bookings. Args: {date? (YYYY-MM-DD), facility_id?}. Returns bookings with time, customer, facility.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date       string `json:"date"`
				FacilityID string `json:"facility_id"`
			}
			_ = json.Unmarshal(args, &p) // all optional
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				sql := `SELECT f.name, b.date::text, b.start_time::text, b.end_time::text, b.customer_name, b.status
				          FROM sports_bookings b JOIN sports_facilities f ON f.id = b.facility_id
				         WHERE b.tenant_id = $1`
				qargs := []any{tenantID}
				idx := 2
				if p.Date != "" {
					sql += fmt.Sprintf(" AND b.date = $%d::date", idx)
					qargs = append(qargs, p.Date)
					idx++
				}
				if p.FacilityID != "" {
					sql += fmt.Sprintf(" AND b.facility_id = $%d::uuid", idx)
					qargs = append(qargs, p.FacilityID)
					idx++
				}
				sql += " ORDER BY b.date, b.start_time LIMIT 20"
				r, err := tx.Query(ctx, sql, qargs...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name, date, st, et, cust, status string
					if err := r.Scan(&name, &date, &st, &et, &cust, &status); err != nil {
						return err
					}
					rows = append(rows, fmt.Sprintf("%s %s %s-%s %s (%s)", date, name, st, et, cust, status))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no bookings found"
			}
			return strings.Join(rows, "\n")
		},
	}
}
