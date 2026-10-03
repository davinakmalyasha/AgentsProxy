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

func init() { agentstools.Register("centrum", "top_customers", topCustomersTool) }

func topCustomersTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "top_customers",
		Description: "Customers ranked by visit count. Use for loyalty and who-visits-most " +
			"questions; it is the only tool that reads the customers table.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"description":"how many customers, default 5"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Limit < 1 {
				p.Limit = 5
			}
			if p.Limit > 20 {
				p.Limit = 20
			}
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				// Name resolution falls back in this order: the directory
				// entry, then the name typed on the booking itself, then a
				// literal. The old query only ever read c.name, so every
				// walk-in whose phone was not in the customer directory
				// came back as "?" â€” the tool filtered on
				// b.customer_name being present, then ignored it.
				//
				// Grouping is by phone, not name: the comment above used
				// to argue for phone and then group by c.name anyway,
				// which both merged two different people who share a name
				// and split one person who appears under a corrected
				// spelling.
				r, err := tx.Query(ctx,
					`SELECT COALESCE(NULLIF(b.customer_phone,''),
					                 NULLIF(c.name,''),
					                 NULLIF(b.customer_name,''),
					                 '?') AS who,
					        MIN(COALESCE(NULLIF(c.name,''), NULLIF(b.customer_name,''), '?')) AS label,
					        COUNT(*) AS visits
					   FROM sports_bookings b
					   LEFT JOIN sports_customers c
					          ON c.tenant_id = b.tenant_id AND c.phone = b.customer_phone
					  WHERE b.tenant_id=$1 AND b.status IN ('confirmed','checked_in','completed')
					    AND (b.customer_name IS NOT NULL AND b.customer_name <> ''
					         OR c.name IS NOT NULL AND c.name <> '')
					  GROUP BY 1
					  ORDER BY visits DESC
					  LIMIT $2`,
					tenantID, p.Limit)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var who, label string
					var visits int
					if err := r.Scan(&who, &label, &visits); err != nil {
						return err
					}
					// Show the phone as well as the name: two bookings
					// under "?" are otherwise indistinguishable, and the
					// whole point of the tool is telling two apart.
					if who != label {
						rows = append(rows, fmt.Sprintf("%s (%s): %d visits", label, who, visits))
					} else {
						rows = append(rows, fmt.Sprintf("%s: %d visits", label, visits))
					}
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no customers yet"
			}
			return strings.Join(rows, "\n")
		},
	}
}
