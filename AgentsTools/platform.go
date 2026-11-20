// Package agentstools platform tools are available to every project. They
// are registered with an empty Tenant so NewToolSetFor includes them for
// every tenant, and they query platform tables (clients) rather than a
// project schema.
package agentstools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	Register("", "tenant_info", func(pool *pgxpool.Pool) Tool {
		return Tool{
			Name:        "tenant_info",
			Description: "Return the current project's name, vertical, and status. Use this to confirm which business you are assisting.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Run: func(ctx context.Context, tenantID string, _ json.RawMessage) string {
				if pool == nil {
					return `{"error":"database unavailable"}`
				}
				var name, vertical, status string
				err := pool.QueryRow(ctx,
					`SELECT name, vertical, status FROM clients WHERE tenant_id = $1`,
					tenantID).Scan(&name, &vertical, &status)
				if err != nil {
					return fmt.Sprintf(`{"error":%q}`, "tenant not found")
				}
				out, _ := json.Marshal(map[string]string{
					"tenant_id": tenantID,
					"name":      name,
					"vertical":  vertical,
					"status":    status,
				})
				return string(out)
			},
		}
	})
}
