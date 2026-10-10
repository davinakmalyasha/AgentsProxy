// Schema contract rollout (Phase D). Per-tenant canary-then-fleet evolution:
// a contract bump is a row update, rollback is a row back. Never a migration.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ContractBump sets one tenant's contract version for a feature (route:
// POST /v1/admin/clients/{id}/contract, wrapped in WithValidID like Patch).
//
// Rules (all fail 400, nothing half-applied):
//   - feature must exist in feature_schema_versions and version must not
//     exceed its max (you cannot roll a tenant past any released version);
//   - version < 1 rejected; status resets to ok (a failed canary is
//     re-bumped explicitly, never silently resurrected).
//
// Rollback = bump back to the previous version. Handlers tolerate the
// previous shape for one release, so rollback is instant and safe.
func (a *Admin) ContractBump(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id required")
		return
	}
	var req struct {
		Feature string `json:"feature"`
		Version int    `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Feature = strings.TrimSpace(req.Feature)
	if req.Feature == "" || req.Version < 1 {
		writeJSONError(w, http.StatusBadRequest, "feature and version >= 1 required")
		return
	}
	var tenantID string
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT tenant_id FROM clients WHERE id = $1::uuid`, id).Scan(&tenantID); err != nil {
		writeJSONError(w, http.StatusNotFound, "unknown tenant")
		return
	}
	var maxVersion int
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(MAX(version), 0) FROM feature_schema_versions WHERE feature = $1`,
		req.Feature).Scan(&maxVersion); err != nil || maxVersion == 0 {
		writeJSONError(w, http.StatusBadRequest, "unknown feature")
		return
	}
	if req.Version > maxVersion {
		writeJSONError(w, http.StatusBadRequest, "version ahead of released catalog")
		return
	}
	if _, err := a.Pool.Exec(r.Context(),
		`INSERT INTO tenant_schema_contracts (tenant_id, feature, version, status, applied_at, error)
		 VALUES ($1, $2, $3, 'ok', now(), '')
		 ON CONFLICT (tenant_id, feature) DO UPDATE
		   SET version = EXCLUDED.version, status = 'ok', applied_at = now(), error = ''`,
		tenantID, req.Feature, req.Version); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": tenantID, "feature": req.Feature, "version": req.Version, "status": "ok",
	})
}
