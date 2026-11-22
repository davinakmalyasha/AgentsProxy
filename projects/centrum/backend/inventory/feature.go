// Package inventory implements the sports-vertical "inventory"
// feature: food/stock items sold at the venue counter. Uses a custom
// table (sports_inventory) because quantity tracking + a low-stock
// threshold need real INT columns and atomic increments.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package inventory

import (
	"encoding/json"
	"errors"
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

const featureName = "inventory"

func init() { Register() }

func Register() {
	features.RegisterProject("centrum", "sports", features.Feature{
		Name:          featureName,
		Kind:          "", // custom table, not tenant_records
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", records.WithValidID(h.getOne))
	r.Patch("/{id}", records.WithValidID(h.patch))
	r.Delete("/{id}", records.WithValidID(h.del))
	r.Post("/{id}/adjust", records.WithValidID(h.adjust))
}

type handler struct {
	pool *pgxpool.Pool
}

type itemResp struct {
	ID                string  `json:"id"`
	TenantID          string  `json:"tenant_id"`
	Name              string  `json:"name"`
	SKU               string  `json:"sku"`
	Quantity          int     `json:"quantity"`
	UnitPrice         float64 `json:"unit_price"`
	LowStockThreshold int     `json:"low_stock_threshold"`
	LowStock          bool    `json:"low_stock"`
	Status            string  `json:"status"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req struct {
		Name              string  `json:"name"`
		SKU               string  `json:"sku"`
		Quantity          *int    `json:"quantity"`
		UnitPrice         float64 `json:"unit_price"`
		LowStockThreshold *int    `json:"low_stock_threshold"`
		Status            string  `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	qty := 0
	if req.Quantity != nil {
		if *req.Quantity < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "quantity must be >= 0")
			return
		}
		qty = *req.Quantity
	}
	threshold := 5
	if req.LowStockThreshold != nil {
		threshold = *req.LowStockThreshold
	}
	if req.UnitPrice < 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "unit_price must be >= 0")
		return
	}
	status := req.Status
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "paused" {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
		return
	}
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_inventory (tenant_id, name, sku, quantity, unit_price, low_stock_threshold, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`,
			idn.TenantID(), req.Name, req.SKU, qty, req.UnitPrice, threshold, status,
		).Scan(&id)
	})
	if err != nil {
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
	out := make([]itemResp, 0, limit+1)
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, tenant_id, name, sku, quantity, unit_price, low_stock_threshold, status, created_at, updated_at
			   FROM sports_inventory WHERE tenant_id = $1 ORDER BY name LIMIT $2`,
			idn.TenantID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it itemResp
			var createdAt, updatedAt time.Time
			if err := rows.Scan(&it.ID, &it.TenantID, &it.Name, &it.SKU, &it.Quantity, &it.UnitPrice,
				&it.LowStockThreshold, &it.Status, &createdAt, &updatedAt); err != nil {
				return err
			}
			it.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			it.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
			it.LowStock = it.Quantity <= it.LowStockThreshold
			out = append(out, it)
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
	var it itemResp
	var createdAt, updatedAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, name, sku, quantity, unit_price, low_stock_threshold, status, created_at, updated_at
			   FROM sports_inventory WHERE id = $1::uuid AND tenant_id = $2`,
			id, idn.TenantID(),
		).Scan(&it.ID, &it.TenantID, &it.Name, &it.SKU, &it.Quantity, &it.UnitPrice,
			&it.LowStockThreshold, &it.Status, &createdAt, &updatedAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	it.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	it.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	it.LowStock = it.Quantity <= it.LowStockThreshold
	records.WriteJSON(w, http.StatusOK, it)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Name              *string  `json:"name"`
		SKU               *string  `json:"sku"`
		UnitPrice         *float64 `json:"unit_price"`
		LowStockThreshold *int     `json:"low_stock_threshold"`
		Status            *string  `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{"updated_at = now()"}
	args := []any{}
	idx := 1
	if req.Name != nil {
		sets = append(sets, "name = $"+strconv.Itoa(idx))
		args = append(args, *req.Name)
		idx++
	}
	if req.SKU != nil {
		sets = append(sets, "sku = $"+strconv.Itoa(idx))
		args = append(args, *req.SKU)
		idx++
	}
	if req.UnitPrice != nil {
		if *req.UnitPrice < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "unit_price must be >= 0")
			return
		}
		sets = append(sets, "unit_price = $"+strconv.Itoa(idx))
		args = append(args, *req.UnitPrice)
		idx++
	}
	if req.LowStockThreshold != nil {
		sets = append(sets, "low_stock_threshold = $"+strconv.Itoa(idx))
		args = append(args, *req.LowStockThreshold)
		idx++
	}
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "paused" {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		sets = append(sets, "status = $"+strconv.Itoa(idx))
		args = append(args, *req.Status)
		idx++
	}
	if len(sets) == 1 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_inventory SET %s WHERE id = $%d::uuid AND tenant_id = $%d",
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
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM sports_inventory WHERE id = $1::uuid AND tenant_id = $2`,
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

// adjust atomically changes quantity: positive = restock, negative =
// sale. Quantity never goes below 0 (the UPDATE's WHERE guard + row
// lock make concurrent sales safe).
func (h *handler) adjust(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Delta int `json:"delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Delta == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "delta must be non-zero")
		return
	}
	var newQty int
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(),
			`UPDATE sports_inventory SET quantity = quantity + $1, updated_at = now()
			  WHERE id = $2::uuid AND tenant_id = $3 AND quantity + $1 >= 0
			  RETURNING quantity`,
			req.Delta, id, idn.TenantID(),
		).Scan(&newQty)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the item doesn't exist or quantity would go negative.
			records.WriteJSONError(w, http.StatusConflict, "insufficient stock or item not found")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "quantity": newQty})
}
