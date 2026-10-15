// DONE: PRD 6.3 (admin CRUD), 6.5 (vertical + features) / STANDARDS A, D, F
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/pgerr"
)

type Admin struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
}

type createClientReq struct {
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
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
	// Default vertical.
	if req.Vertical == "" {
		req.Vertical = "fnb"
	}
	if !features.IsAllowedVertical(req.Vertical) {
		writeJSONError(w, http.StatusBadRequest, "unknown vertical: "+req.Vertical)
		return
	}
	// Default features = DefaultSetFor(vertical).
	if len(req.Features) == 0 {
		req.Features = features.DefaultSetFor(req.Vertical)
	} else {
		// Validate every requested feature is known + allowed for the vertical.
		for _, f := range req.Features {
			if !features.IsKnownFeature(f) {
				writeJSONError(w, http.StatusBadRequest, "unknown feature: "+f)
				return
			}
			if !features.AllowedForVertical(f, req.Vertical) {
				writeJSONError(w, http.StatusBadRequest, "feature "+f+" not allowed for vertical "+req.Vertical)
				return
			}
		}
	}
	if req.MonthlyQuotaTokens == 0 {
		req.MonthlyQuotaTokens = 500000
	}
	if err := records.ValidateQuota(req.MonthlyQuotaTokens); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate origins before creating anything: a bad origin must
	// fail the whole provisioning call, not half of it.
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
	var id string
	err = a.Pool.QueryRow(r.Context(),
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1, $2, $3, $4, 'active', $5, $6) RETURNING id::text`,
		req.Name, req.TenantID, hash, req.MonthlyQuotaTokens, req.Vertical, req.Features,
	).Scan(&id)
	if err != nil {
		if pgerr.IsUniqueViolation(err) {
			writeJSONError(w, http.StatusConflict, "tenant_id or token_hash already exists")
			return
		}
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
	})
	// Register validated origins (created_by marks the provisioning path).
	// Best-effort after the client exists: the tenant is usable without
	// them (env allowlist still merges), and a failed insert here must
	// not roll back provisioning — the admin origins endpoint retries it.
	for _, o := range validOrigins {
		if _, err := a.Pool.Exec(r.Context(),
			`INSERT INTO frontend_origins (tenant_id, origin, label, created_by)
			 VALUES ($1, $2, 'provisioning', 'provisioning')
			 ON CONFLICT (tenant_id, origin) DO NOTHING`,
			req.TenantID, o); err != nil {
			slog.Warn("admin origins provisioning failed", "tenant", req.TenantID, "origin", o, "err", err)
		}
	}
	// A fresh tenant must resolve immediately: clear any negative
	// tenant-known entry so public reads don't 404 it for 30s.
	if err := records.EvictTenantKnown(r.Context(), cache.RDBOf(a.Cache), req.TenantID); err != nil {
		slog.Warn("admin tenant evict failed", "tenant", req.TenantID, "err", err)
	}
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
		`SELECT id::text, name, tenant_id, vertical, features, status, monthly_quota_tokens, created_at
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
		if err := rows.Scan(&c.ID, &c.Name, &c.TenantID, &c.Vertical, &c.Features, &c.Status, &c.MonthlyQuotaTokens, &createdAt); err != nil {
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
		// Validate names + per-vertical allow-list.
		for _, f := range *req.Features {
			if !features.IsKnownFeature(f) {
				writeJSONError(w, http.StatusBadRequest, "unknown feature: "+f)
				return
			}
			if !features.AllowedForVertical(f, curVert) {
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
