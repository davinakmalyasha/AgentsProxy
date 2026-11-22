// DONE: PRD 6.3 (admin CRUD), 6.5 (vertical + features) / STANDARDS A, D, F
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/pgerr"
)

type Admin struct {
	Pool     *pgxpool.Pool
	Cache    *cache.Client
	Projects ProjectCatalog
	Auth     *authapi.Auth
}

var projectSchemaRe = regexp.MustCompile(`^project_[a-z0-9_]{1,55}$`)

type createClientReq struct {
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
	// Schema is the project's business schema. Empty defaults to
	// project_<tenant_id>. It must match project_<slug>; "public" is
	// reserved for the legacy platform tenant row, not project provisioning.
	Schema string `json:"schema"`
	// Origins optionally registers frontend origins in the same call
	// (migration 058): one round trip for new-project onboarding.
	// Validated BEFORE the client insert so a bad origin fails fast
	// without creating a half-provisioned tenant.
	Origins []string `json:"origins"`
}

type createClientResp struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	Token              string   `json:"token"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
	Schema             string   `json:"schema"`
}

type patchClientReq struct {
	MonthlyQuotaTokens *int      `json:"monthly_quota_tokens"`
	Status             *string   `json:"status"`
	Vertical           *string   `json:"vertical"`
	Features           *[]string `json:"features"`
}

type clientListRow struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
	Status             string   `json:"status"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Schema             string   `json:"schema"`
	CreatedAt          string   `json:"created_at"`
}

type usageRow struct {
	ClientID  string  `json:"client_id"`
	Name      string  `json:"name"`
	TenantID  string  `json:"tenant_id"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

func (a *Admin) Create(w http.ResponseWriter, r *http.Request) {
	var req createClientReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Name == "" || req.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "name and tenant_id required")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.TenantID = strings.TrimSpace(req.TenantID)
	if len(req.Name) == 0 || len(req.Name) > 120 {
		writeJSONError(w, http.StatusBadRequest, "invalid name")
		return
	}
	if err := records.ValidateTenantSlug(req.TenantID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// A projects/<slug>/ folder is what makes a tenant a project. Without
	// one the tenant is a platform tenant and lives in public; it must not
	// claim a project_* schema, because that schema has no project tables.
	var project *agentconfig.Project
	if a.Projects != nil {
		p, err := a.Projects.Get(req.TenantID)
		switch {
		case err == nil:
			if err := agentconfig.Validate(p, agentconfig.DefaultBudget()); err != nil {
				slog.Error("admin project config", "tenant", req.TenantID, "err", err)
				writeJSONError(w, http.StatusBadRequest, "project.json is invalid")
				return
			}
			project = p
		case errors.Is(err, ErrProjectNotFound):
			// Platform tenant: no custom code, no project schema.
		default:
			slog.Error("admin project config", "tenant", req.TenantID, "err", err)
			writeJSONError(w, http.StatusBadRequest, "project.json is invalid")
			return
		}
	}
	if project != nil {
		if req.Schema != "" && req.Schema != project.Schema {
			writeJSONError(w, http.StatusBadRequest, "schema does not match project.json")
			return
		}
		req.Schema = project.Schema
		if req.Vertical == "" {
			req.Vertical = project.Vertical
		}
		if req.Vertical != project.Vertical {
			writeJSONError(w, http.StatusBadRequest, "vertical does not match project.json")
			return
		}
	} else {
		if req.Schema == "" {
			req.Schema = "public"
		}
		if req.Schema != "public" {
			writeJSONError(w, http.StatusBadRequest,
				"no projects/"+req.TenantID+" folder; a project schema requires one (use schema=public for a platform tenant)")
			return
		}
	}
	if req.Schema != "public" && !projectSchemaRe.MatchString(req.Schema) {
		writeJSONError(w, http.StatusBadRequest, "schema must match project_<slug> (max 63 characters)")
		return
	}

	// Default vertical.
	if req.Vertical == "" {
		req.Vertical = "fnb"
	}
	if !features.IsAllowedVertical(req.Vertical) {
		writeJSONError(w, http.StatusBadRequest, "unknown vertical: "+req.Vertical)
		return
	}

	// Default features: project.json is the project's declared default set;
	// otherwise the vertical default plus every compiled project feature. A
	// caller may pass a subset, but every name must be known and allowed.
	if len(req.Features) == 0 {
		if project != nil && len(project.Features) > 0 {
			req.Features = append([]string(nil), project.Features...)
		} else {
			req.Features = features.DefaultSetFor(req.Vertical)
			for _, f := range features.ProjectFeatures(req.TenantID) {
				if features.ProjectFeatureAllowed(req.TenantID, f.Name, req.Vertical) &&
					!containsString(req.Features, f.Name) {
					req.Features = append(req.Features, f.Name)
				}
			}
		}
	}
	for _, f := range req.Features {
		if !features.IsKnownFeature(f) && !features.IsKnownProjectFeature(req.TenantID, f) {
			writeJSONError(w, http.StatusBadRequest, "unknown feature: "+f)
			return
		}
		if !featureAllowedForClient(req.TenantID, req.Vertical, f) {
			writeJSONError(w, http.StatusBadRequest, "feature "+f+" not allowed for vertical "+req.Vertical)
			return
		}
	}

	if req.MonthlyQuotaTokens == 0 {
		req.MonthlyQuotaTokens = 500000
	}
	if err := records.ValidateQuota(req.MonthlyQuotaTokens); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// DDL belongs to the deploy step. A project client can only be created
	// after migrate apply has created its schema and recorded every project
	// migration; otherwise the tenant is born pointing at an empty schema.
	if project != nil {
		ready, err := a.Projects.Ready(r.Context(), project)
		if err != nil {
			slog.Error("admin project readiness", "tenant", req.TenantID, "err", err)
			writeJSONError(w, http.StatusConflict,
				"project schema is not provisionable; run migrate apply and migrate check")
			return
		}
		if !ready {
			writeJSONError(w, http.StatusConflict,
				"project schema is not migrated; run migrate apply first")
			return
		}
	}

	// One project schema, one client. The partial unique index is the final
	// guard; this lookup turns the common case into a useful 409 before the
	// insert rather than a generic constraint error. Platform tenants share
	// public, so the check is project-only.
	if req.Schema != "public" {
		var schemaOwner string
		switch err := a.Pool.QueryRow(r.Context(),
			`SELECT tenant_id FROM public.clients
			  WHERE db_schema = $1 AND db_schema LIKE 'project\_%'`, req.Schema).Scan(&schemaOwner); {
		case err == nil:
			writeJSONError(w, http.StatusConflict, "schema already belongs to tenant "+schemaOwner)
			return
		case errors.Is(err, pgx.ErrNoRows):
			// Available.
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
	}

	// Validate origins before creating anything: a bad origin must fail the
	// whole provisioning call, not half of it.
	validOrigins := make([]string, 0, len(req.Origins))
	for _, raw := range req.Origins {
		o, err := middleware.ValidateOrigin(raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		validOrigins = append(validOrigins, o)
	}
	tok, hash, err := auth.GenerateToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token gen")
		return
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	var id string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ($1, $2, $3, $4, 'active', $5, $6, $7) RETURNING id::text`,
		req.Name, req.TenantID, hash, req.MonthlyQuotaTokens, req.Vertical, req.Features, req.Schema,
	).Scan(&id)
	if err != nil {
		if pgerr.IsUniqueViolation(err) {
			writeJSONError(w, http.StatusConflict, "tenant_id, token_hash, or project schema already exists")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, o := range validOrigins {
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO frontend_origins (tenant_id, origin, label, created_by)
			 VALUES ($1, $2, 'provisioning', 'provisioning')
			 ON CONFLICT (tenant_id, origin) DO NOTHING`,
			req.TenantID, o); err != nil {
			slog.Error("admin origins provisioning failed", "tenant", req.TenantID, "origin", o, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "origin provisioning failed")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}

	writeJSON(w, http.StatusCreated, createClientResp{
		ID:                 id,
		Name:               req.Name,
		TenantID:           req.TenantID,
		Token:              tok, // SHOWN ONCE
		MonthlyQuotaTokens: req.MonthlyQuotaTokens,
		Vertical:           req.Vertical,
		Features:           req.Features,
		Schema:             req.Schema,
	})
	// A fresh tenant must resolve immediately: clear any negative
	// tenant-known entry so public reads don't 404 it for 30s.
	if err := records.EvictTenantKnown(r.Context(), cache.RDBOf(a.Cache), req.TenantID); err != nil {
		slog.Warn("admin tenant evict failed", "tenant", req.TenantID, "err", err)
	}
}

type seedOwnerReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone"`
	Role     string `json:"role"` // owner (default) | pegawai
}

// SeedOwner bootstraps the first staff user for an existing client. It is
// admin-key protected, so it does not need an owner session. The project's
// auth provider decides which table the row belongs to; a platform tenant
// with no provider returns 400 rather than silently writing nowhere.
func (a *Admin) SeedOwner(w http.ResponseWriter, r *http.Request) {
	if a.Auth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "staff auth is not configured")
		return
	}
	id := chi.URLParam(r, "id")
	var tenantID, status string
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT tenant_id, status FROM clients WHERE id = $1::uuid`, id).
		Scan(&tenantID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if status != "active" {
		writeJSONError(w, http.StatusConflict, "client is "+status)
		return
	}
	var req seedOwnerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.FullName = strings.TrimSpace(req.FullName)
	if !strings.Contains(req.Email, "@") {
		writeJSONError(w, http.StatusBadRequest, "email invalid")
		return
	}
	if len(req.Password) < 8 {
		writeJSONError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	role := req.Role
	if role == "" {
		role = clientctx.RoleOwner
	}
	if role != clientctx.RoleOwner && role != clientctx.RoleStaff {
		writeJSONError(w, http.StatusBadRequest, "role must be owner or pegawai")
		return
	}
	hash, err := authapi.HashPassword(req.Password)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	u, err := a.Auth.RegisterUser(r.Context(), tenantID, authapi.RegisterInput{
		Email:        req.Email,
		PasswordHash: hash,
		Role:         role,
		FullName:     req.FullName,
		Phone:        req.Phone,
	})
	switch {
	case errors.Is(err, authapi.ErrEmailTaken):
		writeJSONError(w, http.StatusConflict, "email already registered")
		return
	case errors.Is(err, authapi.ErrInvalidRole):
		writeJSONError(w, http.StatusBadRequest, "role must be owner or pegawai")
		return
	case errors.Is(err, authapi.ErrInvalidCredentials):
		writeJSONError(w, http.StatusBadRequest, "staff auth is not configured for this tenant")
		return
	case err != nil:
		slog.Error("admin seed owner", "tenant", tenantID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
}

func (a *Admin) List(w http.ResponseWriter, r *http.Request) {
	// ?limit clamps output (default 200, max 2000) so the client
	// directory can't grow a single unbounded response.
	limit := 200
	if l := strings.TrimSpace(r.URL.Query().Get("limit")); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 2000 {
			n = 2000
		}
		limit = n
	}
	rows, err := a.Pool.Query(r.Context(),
		`SELECT id::text, name, tenant_id, vertical, features, status, monthly_quota_tokens, db_schema, created_at
		   FROM clients ORDER BY created_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []clientListRow{}
	for rows.Next() {
		var c clientListRow
		var createdAt time.Time
		if err := rows.Scan(&c.ID, &c.Name, &c.TenantID, &c.Vertical, &c.Features, &c.Status, &c.MonthlyQuotaTokens, &c.Schema, &createdAt); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		c.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) Patch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id required")
		return
	}
	var req patchClientReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.MonthlyQuotaTokens == nil && req.Status == nil && req.Vertical == nil && req.Features == nil {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	// Read current row for cache eviction + cross-field validation.
	var (
		hash     string
		curVert  string
		tenantID string
	)
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT token_hash, vertical, tenant_id FROM clients WHERE id = $1::uuid`, id,
	).Scan(&hash, &curVert, &tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}

	q := `UPDATE clients SET updated_at = now()`
	args := []any{}
	idx := 1
	if req.MonthlyQuotaTokens != nil {
		// Zero would silently un-meter the client; negatives and absurd
		// values corrupt quota math. Creation defaults instead of zeroing.
		if *req.MonthlyQuotaTokens <= 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid monthly_quota_tokens")
			return
		}
		if err := records.ValidateQuota(*req.MonthlyQuotaTokens); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		q += fmt.Sprintf(", monthly_quota_tokens = $%d", idx)
		args = append(args, *req.MonthlyQuotaTokens)
		idx++
	}
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "paused" && *req.Status != "blocked" {
			writeJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		q += fmt.Sprintf(", status = $%d", idx)
		args = append(args, *req.Status)
		idx++
	}
	if req.Vertical != nil {
		if !features.IsAllowedVertical(*req.Vertical) {
			writeJSONError(w, http.StatusBadRequest, "unknown vertical: "+*req.Vertical)
			return
		}
		q += fmt.Sprintf(", vertical = $%d", idx)
		args = append(args, *req.Vertical)
		idx++
		curVert = *req.Vertical
	}
	if req.Features != nil {
		// Validate names + per-vertical allow-list. A project-owned feature
		// is valid for its own tenant even though it is not in the global
		// registry.
		for _, f := range *req.Features {
			if !features.IsKnownFeature(f) && !features.IsKnownProjectFeature(tenantID, f) {
				writeJSONError(w, http.StatusBadRequest, "unknown feature: "+f)
				return
			}
			if !featureAllowedForClient(tenantID, curVert, f) {
				writeJSONError(w, http.StatusBadRequest, "feature "+f+" not allowed for vertical "+curVert)
				return
			}
		}
		q += fmt.Sprintf(", features = $%d", idx)
		args = append(args, *req.Features)
		idx++
	}
	q += fmt.Sprintf(" WHERE id = $%d::uuid", idx)
	args = append(args, id)
	if _, err := a.Pool.Exec(r.Context(), q, args...); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	// Evict cache so the next request picks up new vertical/features.
	if err := a.Cache.EvictByHash(r.Context(), hash); err != nil {
		slog.Warn("admin cache evict failed", "client_id", id, "err", err)
	}
	// Evict the tenant-known cache too: a paused tenant must fence
	// now, not when the 30s TTL lapses.
	if err := records.EvictTenantKnown(r.Context(), cache.RDBOf(a.Cache), tenantID); err != nil {
		slog.Warn("admin tenant evict failed", "client_id", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"status":   "updated",
		"vertical": curVert,
	})
}

// featureAllowedForClient accepts either a global feature (validated against
// the vertical allow-list) or a feature registered for this project. The
// latter is what lets a new project put its own feature name into
// clients.features without adding it to every vertical's global registry.
func featureAllowedForClient(tenantID, vertical, name string) bool {
	// A project-owned name wins for its own project. The two registries are
	// separate; a project may intentionally own a feature name that also
	// exists in the platform vocabulary.
	if features.IsKnownProjectFeature(tenantID, name) {
		return features.ProjectFeatureAllowed(tenantID, name, vertical)
	}
	return features.AllowedForVertical(name, vertical)
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func (a *Admin) Usage(w http.ResponseWriter, r *http.Request) {
	q := `SELECT c.id::text, c.name, c.tenant_id,
	             COALESCE(SUM(u.tokens_in), 0)::int,
	             COALESCE(SUM(u.tokens_out), 0)::int,
	             COALESCE(SUM(u.cost_usd), 0)::float
	      FROM clients c LEFT JOIN usage u ON u.client_id = c.id
	      WHERE 1=1`
	args := []any{}
	idx := 1
	var from, to time.Time
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid from")
			return
		}
		from = t
		q += fmt.Sprintf(" AND u.created_at >= $%d", idx)
		args = append(args, t)
		idx++
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid to")
			return
		}
		to = t
		q += fmt.Sprintf(" AND u.created_at < $%d", idx)
		args = append(args, t)
		idx++
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		writeJSONError(w, http.StatusBadRequest, "invalid range")
		return
	}
	if v := r.URL.Query().Get("client"); v != "" {
		if err := records.ValidateTenantSlug(strings.TrimSpace(v)); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		q += fmt.Sprintf(" AND c.tenant_id = $%d", idx)
		args = append(args, v)
		idx++
	}
	// Bound the unbounded: with no from/to this LEFT JOINs every usage
	// row of every tenant since day one. Default to the trailing month
	// and cap rows — the dashboard pages by month anyway.
	if from.IsZero() && to.IsZero() {
		to = time.Now().UTC()
		from = to.AddDate(0, -1, 0)
		q += fmt.Sprintf(" AND u.created_at >= $%d AND u.created_at < $%d", idx, idx+1)
		args = append(args, from, to)
		idx += 2
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 500 {
			n = 500
		}
		limit = n
	}
	q += fmt.Sprintf(` GROUP BY c.id, c.name, c.tenant_id ORDER BY c.tenant_id LIMIT %d`, limit)
	rows, err := a.Pool.Query(r.Context(), q, args...)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []usageRow{}
	for rows.Next() {
		var u usageRow
		if err := rows.Scan(&u.ClientID, &u.Name, &u.TenantID, &u.TokensIn, &u.TokensOut, &u.CostUSD); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, out)
}
