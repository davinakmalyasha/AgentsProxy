// Schema contracts (Phase D): per-tenant logical schema versions over a
// shared physical schema. DDL stays global + additive; rollout is a
// contract-row bump (canary first, fleet after), rollback is a row back.
//
// A missing row means "current": no tenant has rows until the first
// canary, so every existing tenant serves exactly as before (pure
// upgrade). A row AHEAD of the code fails closed with ErrContractMismatch
// (mapped to 428), scoped to that tenant only — every other tenant's row
// is untouched. A row BEHIND still serves: additive columns read as NULL.
package records

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrContractMismatch is returned when a tenant's contract version is
// ahead of what this binary's handlers implement. Handlers map it to
// 428 (distinct from 500/503) so operators can tell "rollout gate"
// apart from crashes and fences.
var ErrContractMismatch = errors.New("schema contract mismatch")

// CheckContractTx verifies the tenant's contract for a feature against
// the code's version. Call at handler entry, inside the tenant
// transaction (one indexed SELECT on the primary key).
func CheckContractTx(ctx context.Context, tx pgx.Tx, tenantID, feature string, codeVersion int) error {
	var version int
	var status string
	err := tx.QueryRow(ctx,
		`SELECT version, status FROM tenant_schema_contracts WHERE tenant_id=$1 AND feature=$2`,
		tenantID, feature).Scan(&version, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no row = current shape (pre-canary tenants)
		}
		return err
	}
	if version > codeVersion {
		return ErrContractMismatch
	}
	return nil
}
