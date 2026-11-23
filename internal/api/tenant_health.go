// Tenant quarantine ops (Phase C). Fail-closed per tenant, admin-gated.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

// TenantHealthDeps is what the quarantine endpoints need. Wired in router
// from Deps (ADK runner + admin pool); nil ADK means "report only, no reload".
type TenantHealthDeps struct {
	ADK  *adkrunner.Runner
	Pool *pgxpool.Pool // admin pool: quarantine rows are fenced here too
	// RestFenced is the boot-time bad-project set. TenantReload hot-swaps
	// only the ADK config; a REST router that was fenced at boot remains a
	// 503 until a deploy rebuilds it, and the response must say so.
	RestFenced map[string]error
}

// TenantHealth lists quarantined tenants (slug → reason) plus serving
// slugs. Status only — no prompt bodies, no contacts.
func TenantHealth(deps TenantHealthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"quarantined": tenanthealth.Snapshot()}
		if deps.ADK != nil {
			out["serving"] = deps.ADK.TenantSlugs()
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// TenantReload re-validates one tenant and either promotes it to serving
// (releasing quarantine + hot-swapping its config) or refreshes the
// quarantine reason. This is the fix-forward path that avoids a redeploy.
func TenantReload(deps TenantHealthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ADK == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "agent runner unavailable")
			return
		}
		var req struct {
			TenantID string `json:"tenant_id"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		slug := strings.TrimSpace(req.TenantID)
		if slug == "" {
			writeJSONError(w, http.StatusBadRequest, "tenant_id required")
			return
		}
		dir, err := agentconfig.DirForSlug(slug)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "unknown tenant")
			return
		}
		p, err := agentconfig.LoadSingle(dir)
		if err != nil {
			fenceTenant(r.Context(), deps.Pool, slug, "reload: "+err.Error())
			slog.Warn("tenant reload failed at load", "tenant", slug, "err", err)
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if p.Slug != slug {
			fenceTenant(r.Context(), deps.Pool, slug, "reload: folder claims a different slug")
			writeJSONError(w, http.StatusUnprocessableEntity, "folder claims a different slug")
			return
		}
		if err := agentconfig.Validate(p, agentconfig.DefaultBudget()); err != nil {
			fenceTenant(r.Context(), deps.Pool, slug, "reload: "+err.Error())
			slog.Warn("tenant reload failed at validate", "tenant", slug, "err", err)
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if names := agentstools.NewToolSetFor(nil, slug, "owner").Names(); len(names) == 0 {
			fenceTenant(r.Context(), deps.Pool, slug, "reload: no tools registered")
			writeJSONError(w, http.StatusUnprocessableEntity, "no tools registered")
			return
		}
		deps.ADK.UpdateProject(p)
		releaseTenant(r.Context(), deps.Pool, slug)
		rest := "serving"
		if deps.RestFenced[slug] != nil {
			rest = "deploy_required"
		}
		slog.Info("tenant reloaded", "tenant", slug, "rest", rest)
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant": slug, "status": "agent_serving", "rest": rest, "cold_prefix": true,
		})
	}
}

// fenceTenant fences in both places: the in-memory registry (fast path
// for HTTP/agent) and the tenant_quarantine table (durable, read by the
// Supabase driver). A DB failure pages but does not stop the fence:
// the registry still protects this process.
func fenceTenant(ctx context.Context, pool *pgxpool.Pool, slug, reason string) {
	tenanthealth.Quarantine(slug, reason)
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenant_quarantine (tenant_id, reason) VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO UPDATE SET reason = EXCLUDED.reason, quarantined_at = now()`,
		slug, reason); err != nil {
		slog.Error("quarantine persist failed", "tenant", slug, "err", err)
	}
}

// releaseTenant unfences in both places. Same failure policy as fencing.
func releaseTenant(ctx context.Context, pool *pgxpool.Pool, slug string) {
	tenanthealth.Release(slug)
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_quarantine WHERE tenant_id = $1`, slug); err != nil {
		slog.Error("quarantine release persist failed", "tenant", slug, "err", err)
	}
}
