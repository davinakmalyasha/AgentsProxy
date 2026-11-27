// Package customers implements the "customers" feature: customer
// directory with no-show tracking + visit counts. Port of
// CentrumBadminton's customers.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package customers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/pgerr"
)

const featureName = "customers"

func init() { Register() }

func Register() {
	features.RegisterProject("centrum", "sports", features.Feature{
		Name:          featureName,
		Kind:          "",
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/top", h.top) // top customers by visits (laporan) — before /{id}
	r.Get("/{id}", records.WithValidID(h.getOne))
	r.Patch("/{id}", records.WithValidID(h.patch))
	r.Delete("/{id}", records.WithValidID(h.del))
}

type handler struct {
	pool *pgxpool.Pool
}

type customerResp struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	NoShowCount int    `json:"no_show_count"`
	Visits      int    `json:"visits"`
	Notes       string `json:"notes"`
	CreatedAt   string `json:"created_at"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "phone required")
		return
	}
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_customers (tenant_id, name, phone, notes)
			 VALUES ($1,$2,$3,$4) RETURNING id::text`,
			idn.TenantID(), req.Name, req.Phone, req.Notes).Scan(&id)
	})
	if err != nil {
		if pgerr.IsUniqueViolation(err) {
			records.WriteJSONError(w, http.StatusConflict, "customer with that phone already exists")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	out := make([]customerResp, 0, limit+1)
	// Paginate the customers FIRST, then aggregate only that page. The
	// old shape joined every customer against the tenant's whole booking
	// history, grouped, and discarded all but LIMIT rows — LIMIT bounded
	// the output, not the work. Match by FK first (023), falling back to
	// the legacy name/phone match for pre-FK rows; COUNT(DISTINCT) so a
	// booking matching on both never double-counts.
	truncated := false
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`WITH page AS (
			   SELECT id, tenant_id, name, phone, no_show_count, notes, created_at
			     FROM sports_customers
			    WHERE tenant_id = $1 AND deleted_at IS NULL
			    ORDER BY name
			    LIMIT $2
			 )
			 SELECT p.id::text, p.tenant_id, p.name, p.phone, p.no_show_count, p.notes, p.created_at,
			        COUNT(DISTINCT b.id) FILTER (WHERE b.status IN ('confirmed','checked_in','completed'))
			   FROM page p
			   LEFT JOIN sports_bookings b
			          ON b.tenant_id = p.tenant_id
			         AND (b.customer_id = p.id
			              OR b.customer_name = p.name
			              OR (p.phone <> '' AND b.customer_phone = p.phone))
			  GROUP BY p.id, p.tenant_id, p.name, p.phone, p.no_show_count, p.notes, p.created_at
			  ORDER BY p.name`,
			idn.TenantID(), limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c customerResp
			var createdAt time.Time
			if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.NoShowCount, &c.Notes, &createdAt, &c.Visits); err != nil {
				return err
			}
			c.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, c)
			if len(out) > limit {
				out = out[:limit]
				truncated = true
				break
			}
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if out == nil {
		out = []customerResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "truncated": truncated})
}

// top returns the top customers by visit count (laporan page).
func (h *handler) top(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	limit := 10
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n >= 1 && n <= 50 {
			limit = n
		}
	}
	out := make([]customerResp, 0, limit)
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT c.id::text, c.tenant_id, c.name, c.phone, c.no_show_count, c.notes, c.created_at,
			        COUNT(DISTINCT b.id) FILTER (WHERE b.status IN ('confirmed','checked_in','completed')) AS visits
			   FROM sports_customers c
			   LEFT JOIN sports_bookings b
			          ON b.tenant_id = c.tenant_id
			         AND (b.customer_id = c.id
			              OR b.customer_name = c.name
			              OR (c.phone <> '' AND b.customer_phone = c.phone))
			  WHERE c.tenant_id = $1 AND c.deleted_at IS NULL
			  GROUP BY c.id
			  ORDER BY visits DESC, c.name
			  LIMIT $2`,
			idn.TenantID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c customerResp
			var createdAt time.Time
			if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.NoShowCount, &c.Notes, &createdAt, &c.Visits); err != nil {
				return err
			}
			c.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getOne(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var c customerResp
	var createdAt time.Time
	err := records.WithTenantRead(r.Context(), h.pool, idn.TenantID(),
		`SELECT c.id::text, c.tenant_id, c.name, c.phone, c.no_show_count, c.notes, c.created_at,
		        (SELECT COUNT(DISTINCT b.id) FROM sports_bookings b
		          WHERE b.tenant_id=c.tenant_id AND b.status IN ('confirmed','checked_in','completed')
		            AND (b.customer_id=c.id
		                 OR b.customer_name=c.name
		                 OR (c.phone <> '' AND b.customer_phone=c.phone)))
		   FROM sports_customers c WHERE c.id=$1::uuid AND c.tenant_id=$2 AND c.deleted_at IS NULL`,
		[]any{id, idn.TenantID()},
		func(rows pgx.Rows) error {
			if !rows.Next() {
				if err := rows.Err(); err != nil {
					return err
				}
				return pgx.ErrNoRows
			}
			return rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.NoShowCount, &c.Notes, &createdAt, &c.Visits)
		})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	c.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, c)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Name  *string `json:"name"`
		Phone *string `json:"phone"`
		Notes *string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{}
	args := []any{}
	idx := 1
	if req.Name != nil {
		sets = append(sets, "name = $"+strconv.Itoa(idx))
		args = append(args, *req.Name)
		idx++
	}
	if req.Phone != nil {
		sets = append(sets, "phone = $"+strconv.Itoa(idx))
		args = append(args, *req.Phone)
		idx++
	}
	if req.Notes != nil {
		sets = append(sets, "notes = $"+strconv.Itoa(idx))
		args = append(args, *req.Notes)
		idx++
	}
	if len(sets) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_customers SET %s WHERE id=$%d::uuid AND tenant_id=$%d AND deleted_at IS NULL",
		strings.Join(sets, ", "), idx, idx+1)
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
		if pgerr.IsUniqueViolation(err) {
			records.WriteJSONError(w, http.StatusConflict, "customer with that phone already exists")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
}

func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Tombstone, not DELETE: the directory row is preserved with
		// who/when, hidden from lists/reads.
		deletedBy := idn.UserID
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_customers SET deleted_at = now(), deleted_by = $3
			  WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
			id, idn.TenantID(), deletedBy)
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
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}
