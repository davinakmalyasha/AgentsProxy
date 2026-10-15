package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// CORS origin registry admin surface (migration 058). One INSERT
// onboards a frontend deployment: no code change, no redeploy.
// RequireAdmin gates the whole /admin tree; writes go through the
// admin pool (runtime role holds SELECT only, by design).

type originWriteReq struct {
	TenantID string `json:"tenant_id"`
	Origin   string `json:"origin"`
	Label    string `json:"label"`
}

type originRow struct {
	TenantID  string `json:"tenant_id"`
	Origin    string `json:"origin"`
	Label     string `json:"label"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

type suffixWriteReq struct {
	Suffix string `json:"suffix"`
	Note   string `json:"note"`
}

type suffixRow struct {
	Suffix    string `json:"suffix"`
	Note      string `json:"note"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// OriginsList returns exact origins (optionally ?tenant_id=) plus all
// suffix rules. Bounded: registries are small by construction, but the
// list still caps at 500 rows per section, not the table.
func (a *Admin) OriginsList(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	tenant := qv.Get("tenant_id")
	rows, err := a.Pool.Query(r.Context(),
		`SELECT tenant_id, origin, label, created_by, created_at
		   FROM frontend_origins
		  WHERE ($1 = '' OR tenant_id = $1)
		  ORDER BY tenant_id, origin LIMIT 500`,
		tenant,
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []originRow{}
	for rows.Next() {
		var o originRow
		var created time.Time
		if err := rows.Scan(&o.TenantID, &o.Origin, &o.Label, &o.CreatedBy, &created); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		o.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	srows, err := a.Pool.Query(r.Context(),
		`SELECT suffix, note, created_by, created_at FROM origin_suffix_rules ORDER BY suffix LIMIT 500`)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	defer srows.Close()
	sout := []suffixRow{}
	for srows.Next() {
		var s suffixRow
		var created time.Time
		if err := srows.Scan(&s.Suffix, &s.Note, &s.CreatedBy, &created); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		s.CreatedAt = created.UTC().Format(time.RFC3339)
		sout = append(sout, s)
	}
	if err := srows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"origins": out, "suffixes": sout})
}

// OriginsCreate registers one frontend origin. Validation rejects
// wildcards, non-https (except loopback), and paths — a bad row must
// never widen the allowlist.
func (a *Admin) OriginsCreate(w http.ResponseWriter, r *http.Request) {
	var req originWriteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	origin, err := middleware.ValidateOrigin(req.Origin)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.Pool.Exec(r.Context(),
		`INSERT INTO frontend_origins (tenant_id, origin, label, created_by)
		 VALUES ($1, $2, $3, 'admin-api')
		 ON CONFLICT (tenant_id, origin) DO NOTHING`,
		req.TenantID, origin, req.Label); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"tenant_id": req.TenantID, "origin": origin})
}

// OriginsDelete removes one registration (query params: ?tenant_id= &origin=).
// Deleting the last origin serving a live frontend closes its browser
// flows — that is the point (offboarding), not an accident to guard.
func (a *Admin) OriginsDelete(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	tenant, raw := qv.Get("tenant_id"), qv.Get("origin")
	if tenant == "" || raw == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_id and origin required")
		return
	}
	origin, err := middleware.ValidateOrigin(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	tag, err := a.Pool.Exec(r.Context(),
		`DELETE FROM frontend_origins WHERE tenant_id = $1 AND origin = $2`,
		tenant, origin)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSONError(w, http.StatusNotFound, "no such registration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": origin})
}

// SuffixCreate registers a dot-boundary suffix rule (".example.com").
// Tenant-less by design: it covers conventional subdomains across all
// tenants, so it stays rare and audited. Exact origins are the norm.
func (a *Admin) SuffixCreate(w http.ResponseWriter, r *http.Request) {
	var req suffixWriteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	suf, err := middleware.ParseSuffix(req.Suffix)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.Pool.Exec(r.Context(),
		`INSERT INTO origin_suffix_rules (suffix, note, created_by)
		 VALUES ($1, $2, 'admin-api')
		 ON CONFLICT (suffix) DO NOTHING`,
		suf, req.Note); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"suffix": suf})
}

// SuffixDelete removes a rule (?suffix=.example.com).
func (a *Admin) SuffixDelete(w http.ResponseWriter, r *http.Request) {
	suf, err := middleware.ParseSuffix(r.URL.Query().Get("suffix"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	tag, err := a.Pool.Exec(r.Context(),
		`DELETE FROM origin_suffix_rules WHERE suffix = $1`, suf)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSONError(w, http.StatusNotFound, "no such rule")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": suf})
}
