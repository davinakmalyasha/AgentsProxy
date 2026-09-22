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
)

const featureName = "customers"

func init() { Register() }

func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "",
		DefaultOn:     false,
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
		if isUniqueViolation(err) {
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
	var out []customerResp
	// One grouped query instead of a per-row correlated count over the
	// whole booking history. Match by name OR phone: the customer's
	// phone is the unique key, but legacy/walk-in bookings may carry
	// only a name — preserve the old name-based count while also
	// counting phone-matched rows. COUNT(DISTINCT) so a booking that
	// matches on both never double-counts.
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT c.id::text, c.tenant_id, c.name, c.phone, c.no_show_count, c.notes, c.created_at,
			        COUNT(DISTINCT b.id) FILTER (WHERE b.status IN ('confirmed','checked_in','completed'))
			   FROM sports_customers c
			   LEFT JOIN sports_bookings b
			          ON b.tenant_id = c.tenant_id
			         AND (b.customer_name = c.name OR (c.phone <> '' AND b.customer_phone = c.phone))
			  WHERE c.tenant_id = $1
			  GROUP BY c.id
			  ORDER BY c.name`,
			idn.TenantID())
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
	var out []customerResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT c.id::text, c.tenant_id, c.name, c.phone, c.no_show_count, c.notes, c.created_at,
			        COUNT(DISTINCT b.id) FILTER (WHERE b.status IN ('confirmed','checked_in','completed')) AS visits
			   FROM sports_customers c
			   LEFT JOIN sports_bookings b
			          ON b.tenant_id = c.tenant_id
			         AND (b.customer_name = c.name OR (c.phone <> '' AND b.customer_phone = c.phone))
			  WHERE c.tenant_id = $1
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
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT c.id::text, c.tenant_id, c.name, c.phone, c.no_show_count, c.notes, c.created_at,
			        (SELECT COUNT(DISTINCT b.id) FROM sports_bookings b
			          WHERE b.tenant_id=c.tenant_id AND b.status IN ('confirmed','checked_in','completed')
			            AND (b.customer_name=c.name OR (c.phone <> '' AND b.customer_phone=c.phone)))
			   FROM sports_customers c WHERE c.id=$1::uuid AND c.tenant_id=$2`,
			id, idn.TenantID(),
		).Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.NoShowCount, &c.Notes, &createdAt, &c.Visits)
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
	sql := fmt.Sprintf("UPDATE sports_customers SET %s WHERE id=$%d::uuid AND tenant_id=$%d",
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
		if isUniqueViolation(err) {
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
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM sports_customers WHERE id=$1::uuid AND tenant_id=$2`,
			id, idn.TenantID())
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

func isUniqueViolation(err error) bool {
	if pgErr, ok := err.(interface{ SQLState() string }); ok {
		return pgErr.SQLState() == "23505"
	}
	return false
}
