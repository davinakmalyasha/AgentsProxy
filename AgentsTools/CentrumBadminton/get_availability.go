// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"time"
)

func init() { agentstools.Register("get_availability", getAvailabilityTool) }

func getAvailabilityTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name:        "get_availability",
		Description: "Get busy slots for a date. Args: {date (YYYY-MM-DD), type? (facility type, default badminton)}. Returns facilities with busy intervals.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
				Type string `json:"type"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.Date == "" {
				return "error: date required YYYY-MM-DD"
			}
			if _, err := time.Parse("2006-01-02", p.Date); err != nil {
				return "error: date must be YYYY-MM-DD"
			}
			typ := strings.ToLower(strings.TrimSpace(p.Type))
			if typ == "" {
				typ = "badminton"
			}
			var out []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				// One grouped query instead of a per-facility round-trip:
				// facilities LEFT JOIN that day's active bookings, then
				// aggregate busy windows per facility. Times are cast to
				// text and truncated to HH:MM in SQL so no Go parsing.
				rows, err := tx.Query(ctx,
					`SELECT f.name,
					        COALESCE(string_agg(to_char(b.start_time,'HH24:MI') || '-' || to_char(b.end_time,'HH24:MI'), ', ' ORDER BY b.start_time), '')
					   FROM sports_facilities f
					   LEFT JOIN sports_bookings b
					          ON b.facility_id = f.id AND b.tenant_id = f.tenant_id
					         AND b.date = $3::date AND b.status NOT IN ('cancelled','no_show')
					  WHERE f.tenant_id=$1 AND f.status='active' AND f.type=$2
					  GROUP BY f.id, f.name
					  ORDER BY f.name`,
					tenantID, typ, p.Date)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var name, busyAgg string
					if err := rows.Scan(&name, &busyAgg); err != nil {
						return err
					}
					if busyAgg == "" {
						out = append(out, name+": kosong semua")
						continue
					}
					out = append(out, name+": "+busyAgg)
				}
				return rows.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(out) == 0 {
				return "no active facilities of type " + typ
			}
			return strings.Join(out, "\n")
		},
	}
}
