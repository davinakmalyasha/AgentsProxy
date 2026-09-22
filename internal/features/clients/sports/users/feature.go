// Package users implements the sports-vertical "users" feature: a
// SAFE read model over sports_users (staff directory + own profile).
// password_hash is NEVER selected or returned. Writes to this table
// happen via /v1/auth/register (create) and PATCH here (owner-only
// active toggle / rename / role change).
//
// C2 fix: the frontend "profiles" table maps here; without it every
// role lookup 404s and all staff fall back to pegawai.
//
// DONE: B2 (profiles contract) / STANDARDS A, D, F
package users

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "users"

func init() { Register() }

func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "", // custom table
		DefaultOn:     false,
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	r.Get("/", h.list)
	r.Get("/{id}", records.WithValidID(h.getOne))
	r.Patch("/{id}", records.WithValidID(h.patch))
}

type handler struct {
	pool *pgxpool.Pool
}

// userResp is the safe projection — no password_hash, ever.
type userResp struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	FullName  string `json:"full_name"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

const safeCols = "id::text, email, role, full_name, is_active, created_at"

func scanUser(rows pgx.Rows) (userResp, time.Time, error) {
	var u userResp
	var createdAt time.Time
	err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.FullName, &u.IsActive, &createdAt)
	return u, createdAt, err
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	q := r.URL.Query()
	filterID := strings.TrimSpace(q.Get("id"))
	filterEmail := strings.ToLower(strings.TrimSpace(q.Get("email")))
	// Non-owners see ONLY themselves (layout profile lookup + is_active
	// gate must work for pegawai, but no staff-directory enumeration).
	onlySelf := !idn.IsOwner()
	var out []userResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		sql := `SELECT ` + safeCols + ` FROM sports_users WHERE tenant_id = $1`
		args := []any{idn.TenantID()}
		if onlySelf && idn.UserID != "" {
			sql += ` AND id = $2::uuid`
			args = append(args, idn.UserID)
		} else {
			if filterID != "" {
				sql += ` AND id = $2::uuid`
				args = append(args, filterID)
			} else if filterEmail != "" {
				sql += ` AND lower(email) = $2`
				args = append(args, filterEmail)
			}
		}
		sql += ` ORDER BY created_at`
		rows, err := tx.Query(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, createdAt, err := scanUser(rows)
			if err != nil {
				return err
			}
			u.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, u)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if out == nil {
		out = []userResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getOne(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	// Non-owners may only fetch themselves.
	if !idn.IsOwner() && (idn.UserID == "" || id != idn.UserID) {
		records.WriteJSONError(w, http.StatusForbidden, "owner only")
		return
	}
	var u userResp
	var createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT `+safeCols+` FROM sports_users WHERE id = $1::uuid AND tenant_id = $2`,
			id, idn.TenantID()).Scan(&u.ID, &u.Email, &u.Role, &u.FullName, &u.IsActive, &createdAt)
	})
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			records.WriteJSONError(w, http.StatusNotFound, "not found")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	u.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, u)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	if !idn.IsOwner() {
		records.WriteJSONError(w, http.StatusForbidden, "owner only")
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		IsActive *bool   `json:"is_active"`
		FullName *string `json:"full_name"`
		Role     *string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.IsActive != nil && !*req.IsActive && id == idn.UserID && idn.UserID != "" {
		records.WriteJSONError(w, http.StatusBadRequest, "cannot deactivate yourself")
		return
	}
	if req.Role != nil && *req.Role != "owner" && *req.Role != "pegawai" {
		records.WriteJSONError(w, http.StatusBadRequest, "role must be owner or pegawai")
		return
	}
	sets := []string{"updated_at = now()"}
	args := []any{}
	idx := 1
	if req.IsActive != nil {
		sets = append(sets, "is_active = $"+strconv.Itoa(idx))
		args = append(args, *req.IsActive)
		idx++
	}
	if req.FullName != nil {
		name := strings.TrimSpace(*req.FullName)
		if name == "" {
			records.WriteJSONError(w, http.StatusBadRequest, "full_name required")
			return
		}
		sets = append(sets, "full_name = $"+strconv.Itoa(idx))
		args = append(args, name)
		idx++
	}
	if req.Role != nil {
		sets = append(sets, "role = $"+strconv.Itoa(idx))
		args = append(args, *req.Role)
		idx++
	}
	if len(sets) == 1 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := `UPDATE sports_users SET ` + strings.Join(sets, ", ") +
		` WHERE id = $` + strconv.Itoa(idx) + `::uuid AND tenant_id = $` + strconv.Itoa(idx+1)
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
}
