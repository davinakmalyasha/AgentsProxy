// Package kantin implements the "kantin" feature: a canteen POS with
// products (cost + stock), sales, and stock movements. The sale is
// ATOMIC in one WithTenantTx: total is computed server-side from DB
// prices (never trusted from the client), stock is decremented, and
// movements are logged. Port of CentrumBadminton's 0005_kantin.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package kantin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "kantin"

func init() { Register() }

func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "",
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	// Products
	r.Post("/products", h.createProduct)
	r.Get("/products", h.listProducts)
	r.Get("/products/{id}", records.WithValidID(h.getProduct))
	r.Patch("/products/{id}", records.WithValidID(h.patchProduct))
	r.Delete("/products/{id}", records.WithValidID(h.delProduct))
	r.Post("/products/{id}/stock", records.WithValidID(h.adjustStock))
	// Stock movement ledger. The app used to point its
	// `kantin_stock_movements` table at this feature's products endpoint,
	// because no movements endpoint existed — so the "Riwayat
	// Pergerakan" list rendered PRODUCT rows as movements: no delta, no
	// reason, no note, and the product's creation date as the movement
	// time. It read like data, so nobody noticed it was fabricated.
	r.Get("/movements", h.listMovements)
	// Optional ?product_id=<uuid> filter.
	r.Get("/products/{id}/movements", records.WithValidID(h.listProductMovements))
	// Sales
	r.Post("/sales", h.createSale) // the atomic sale
	r.Get("/sales", h.listSales)
	r.Get("/sales/{id}", records.WithValidID(h.getSale))
	r.Get("/sales/{id}/items", records.WithValidID(h.listSaleItems))
	r.Post("/sales/{id}/void", records.WithValidID(h.voidSale))
}

type handler struct {
	pool *pgxpool.Pool
}

// ---- products ----

type productResp struct {
	ID                string  `json:"id"`
	TenantID          string  `json:"tenant_id"`
	Name              string  `json:"name"`
	Category          string  `json:"category"`
	Price             float64 `json:"price"`
	CostPrice         float64 `json:"cost_price"`
	TrackStock        bool    `json:"track_stock"`
	Stock             int     `json:"stock"`
	LowStockThreshold int     `json:"low_stock_threshold"`
	IsActive          bool    `json:"is_active"`
	CreatedAt         string  `json:"created_at"`
}

func (h *handler) createProduct(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req struct {
		Name              string  `json:"name"`
		Category          string  `json:"category"`
		Price             float64 `json:"price"`
		CostPrice         float64 `json:"cost_price"`
		TrackStock        *bool   `json:"track_stock"`
		Stock             *int    `json:"stock"`
		LowStockThreshold *int    `json:"low_stock_threshold"`
		SortOrder         *int    `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	if req.Price < 0 || req.CostPrice < 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "price/cost must be >= 0")
		return
	}
	track := false
	if req.TrackStock != nil {
		track = *req.TrackStock
	}
	stock := 0
	if req.Stock != nil {
		stock = *req.Stock
	}
	thr := 5
	if req.LowStockThreshold != nil {
		thr = *req.LowStockThreshold
	}
	cat := req.Category
	if cat == "" {
		cat = "lainnya"
	}
	sortOrder := 0
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_kantin_products (tenant_id, name, category, price, cost_price, track_stock, stock, low_stock_threshold, sort_order)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
			idn.TenantID(), req.Name, cat, req.Price, req.CostPrice, track, stock, thr, sortOrder).Scan(&id)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *handler) listProducts(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var out []productResp
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, tenant_id, name, category, price, cost_price, track_stock, stock, low_stock_threshold, is_active, created_at
			   FROM sports_kantin_products WHERE tenant_id=$1 ORDER BY name LIMIT $2`,
			idn.TenantID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p productResp
			var createdAt time.Time
			if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, &p.Category, &p.Price, &p.CostPrice,
				&p.TrackStock, &p.Stock, &p.LowStockThreshold, &p.IsActive, &createdAt); err != nil {
				return err
			}
			p.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getProduct(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var p productResp
	var createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, name, category, price, cost_price, track_stock, stock, low_stock_threshold, is_active, created_at
			   FROM sports_kantin_products WHERE id=$1::uuid AND tenant_id=$2`,
			id, idn.TenantID(),
		).Scan(&p.ID, &p.TenantID, &p.Name, &p.Category, &p.Price, &p.CostPrice,
			&p.TrackStock, &p.Stock, &p.LowStockThreshold, &p.IsActive, &createdAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, p)
}

func (h *handler) patchProduct(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Name       *string  `json:"name"`
		Category   *string  `json:"category"`
		Price      *float64 `json:"price"`
		CostPrice  *float64 `json:"cost_price"`
		TrackStock *bool    `json:"track_stock"`
		LowStock   *int     `json:"low_stock_threshold"`
		IsActive   *bool    `json:"is_active"`
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
	if req.Category != nil {
		sets = append(sets, "category = $"+strconv.Itoa(idx))
		args = append(args, *req.Category)
		idx++
	}
	if req.Price != nil {
		if *req.Price < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "price must be >= 0")
			return
		}
		sets = append(sets, "price = $"+strconv.Itoa(idx))
		args = append(args, *req.Price)
		idx++
	}
	if req.CostPrice != nil {
		if *req.CostPrice < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "cost must be >= 0")
			return
		}
		sets = append(sets, "cost_price = $"+strconv.Itoa(idx))
		args = append(args, *req.CostPrice)
		idx++
	}
	if req.TrackStock != nil {
		sets = append(sets, "track_stock = $"+strconv.Itoa(idx))
		args = append(args, *req.TrackStock)
		idx++
	}
	if req.LowStock != nil {
		sets = append(sets, "low_stock_threshold = $"+strconv.Itoa(idx))
		args = append(args, *req.LowStock)
		idx++
	}
	if req.IsActive != nil {
		sets = append(sets, "is_active = $"+strconv.Itoa(idx))
		args = append(args, *req.IsActive)
		idx++
	}
	if len(sets) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_kantin_products SET %s WHERE id=$%d::uuid AND tenant_id=$%d",
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

func (h *handler) delProduct(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM sports_kantin_products WHERE id=$1::uuid AND tenant_id=$2`,
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

// adjustStock changes stock directly (restock/waste/correction).
func (h *handler) adjustStock(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Delta  int    `json:"delta"`
		Reason string `json:"reason"`
		Note   string `json:"note"`
		// Count is a physical stock-take ("opname"): set the level to what
		// was actually counted, and let the ledger record the difference.
		//
		// Without this, the UI's "= Stok opname" option sent an ordinary
		// additive delta, so counting 3 units when the system believed 20
		// produced 23 — and the page's promise that "sistem menghitung
		// selisihnya otomatis" was false. A pointer distinguishes
		// "not supplied" from a legitimate count of 0.
		Count *int `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Count != nil {
		if *req.Count < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "count must be zero or more")
			return
		}
		if req.Reason == "" {
			req.Reason = "correction"
		}
	} else if req.Delta == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "delta must be non-zero")
		return
	}
	if req.Reason == "" {
		req.Reason = "correction"
	}
	var newStock int
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		if req.Count != nil {
			// Physical count: read the believed level under a row lock,
			// set the counted one, and record the signed difference so the
			// ledger still explains how the number moved.
			var prev int
			if err := tx.QueryRow(r.Context(),
				`SELECT stock FROM sports_kantin_products
				  WHERE id=$1::uuid AND tenant_id=$2 FOR UPDATE`,
				id, idn.TenantID()).Scan(&prev); err != nil {
				return err
			}
			if err := tx.QueryRow(r.Context(),
				`UPDATE sports_kantin_products SET stock=$1
				  WHERE id=$2::uuid AND tenant_id=$3 RETURNING stock`,
				*req.Count, id, idn.TenantID()).Scan(&newStock); err != nil {
				return err
			}
			if delta := *req.Count - prev; delta != 0 {
				if _, err := tx.Exec(r.Context(),
					`INSERT INTO sports_kantin_stock_movements (tenant_id, product_id, delta, reason, note)
					 VALUES ($1,$2::uuid,$3,$4,$5)`,
					idn.TenantID(), id, delta, req.Reason, req.Note); err != nil {
					return err
				}
			}
			return nil
		}
		err := tx.QueryRow(r.Context(), `UPDATE sports_kantin_products SET stock = stock + $1 WHERE id=$2::uuid AND tenant_id=$3 AND stock + $1 >= 0 RETURNING stock`,
			req.Delta, id, idn.TenantID()).Scan(&newStock)
		if err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO sports_kantin_stock_movements (tenant_id, product_id, delta, reason, note) VALUES ($1,$2::uuid,$3,$4,$5)`,
			idn.TenantID(), id, req.Delta, req.Reason, req.Note)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			records.WriteJSONError(w, http.StatusConflict, "insufficient stock or product not found")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "stock": newStock})
}

type movementResp struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	ProductID  string `json:"product_id"`
	Product    string `json:"product"`
	Delta      int    `json:"delta"`
	Reason     string `json:"reason"`
	Note       string `json:"note"`
	StockAfter int    `json:"stock_after"`
	CreatedAt  string `json:"created_at"`
}

// stock_after is reconstructed from the product's CURRENT stock minus every
// movement recorded after this one. The ledger is append-only per product,
// so this is exact. Returning it as a literal 0 would be worse than not
// returning it at all — a zero on every row reads as "stock hit zero".
const movementSelect = `SELECT m.id::text, m.tenant_id, m.product_id::text, p.name,
                                m.delta, m.reason, m.note, m.created_at,
                                COALESCE(p.stock, 0) - COALESCE((
                                  SELECT SUM(m2.delta)
                                    FROM sports_kantin_stock_movements m2
                                   WHERE m2.tenant_id = m.tenant_id
                                     AND m2.product_id = m.product_id
                                     AND (m2.created_at, m2.id) > (m.created_at, m.id)
                                ), 0)
                           FROM sports_kantin_stock_movements m
                           JOIN sports_kantin_products p
                             ON p.id = m.product_id AND p.tenant_id = m.tenant_id
                          WHERE m.tenant_id = $1`

// listMovements returns the stock ledger, newest first. `stock_after` is
// the level each movement left behind, so the ledger can be read on its own.
func (h *handler) listMovements(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	if pid := strings.TrimSpace(r.URL.Query().Get("product_id")); pid != "" {
		h.writeMovements(w, r, idn.TenantID(), pid, limit)
		return
	}
	h.writeMovements(w, r, idn.TenantID(), "", limit)
}

func (h *handler) listProductMovements(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	h.writeMovements(w, r, idn.TenantID(), chi.URLParam(r, "id"), limit)
}

func (h *handler) writeMovements(w http.ResponseWriter, r *http.Request, tenant, productID string, limit int) {
	var out []movementResp
	err := records.WithTenantTx(r.Context(), h.pool, tenant, func(tx pgx.Tx) error {
		sql := movementSelect
		args := []any{tenant}
		if productID != "" {
			sql += " AND m.product_id = $2::uuid"
			args = append(args, productID)
		}
		sql += fmt.Sprintf(" ORDER BY m.created_at DESC, m.id DESC LIMIT %d", limit)
		rows, err := tx.Query(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m movementResp
			var createdAt time.Time
			if err := rows.Scan(&m.ID, &m.TenantID, &m.ProductID, &m.Product,
				&m.Delta, &m.Reason, &m.Note, &createdAt, &m.StockAfter); err != nil {
				return err
			}
			m.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if out == nil {
		out = []movementResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ---- sales ----

type saleItemReq struct {
	ProductID string `json:"product_id"`
	Qty       int    `json:"qty"`
}

type saleReq struct {
	Method       string        `json:"method"`
	CustomerName string        `json:"customer_name"`
	Note         string        `json:"note"`
	BookingID    string        `json:"booking_id"`
	Items        []saleItemReq `json:"items"`
}

// createSale is the ATOMIC sale. Total is computed from DB prices;
// stock is decremented; movements logged. One transaction.
func (h *handler) createSale(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req saleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(req.Items) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "items required")
		return
	}
	if req.Method == "" {
		req.Method = "cash"
	}
	if req.Method != "cash" && req.Method != "qris" {
		records.WriteJSONError(w, http.StatusBadRequest, "method must be cash or qris")
		return
	}
	// Idempotency-Key (optional phase): a repeated key returns the original
	// sale instead of charging twice. Absent header behaves as before.
	idemKey, haveIdem, idemErr := records.IdempotencyKey(w, r)
	if idemErr != nil {
		return
	}
	if haveIdem {
		var existingID string
		var existingTotal float64
		qerr := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT id::text, total FROM sports_kantin_sales WHERE tenant_id=$1 AND idempotency_key=$2`,
				idn.TenantID(), idemKey).Scan(&existingID, &existingTotal)
		})
		if qerr == nil {
			records.WriteJSON(w, http.StatusOK, map[string]any{"id": existingID, "total": existingTotal, "deduplicated": true})
			return
		}
		if qerr != pgx.ErrNoRows {
			records.WriteJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
	}

	var saleID string
	var total float64
	recordedBy := idn.UserID
	var recordedByArg any
	if recordedBy == "" {
		recordedByArg = nil
	} else {
		recordedByArg = recordedBy
	}
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Insert the sale header (total computed below).
		if err := tx.QueryRow(r.Context(),
			`INSERT INTO sports_kantin_sales (tenant_id, total, method, customer_name, note, booking_id, recorded_by, idempotency_key)
			 VALUES ($1,0,$2,$3,$4,NULLIF($5,'')::uuid,$6,NULLIF($7,'')) RETURNING id::text`,
			idn.TenantID(), req.Method, req.CustomerName, req.Note, req.BookingID, recordedByArg, idemKey).Scan(&saleID); err != nil {
			return err
		}
		for _, it := range req.Items {
			if it.Qty < 1 || it.Qty > 99 {
				return errors.New("bad qty")
			}
			var name string
			var price, cost float64
			var trackStock bool
			var stock int
			err := tx.QueryRow(r.Context(),
				`SELECT name, price, cost_price, track_stock, stock FROM sports_kantin_products
				  WHERE id=$1::uuid AND tenant_id=$2 AND is_active`,
				it.ProductID, idn.TenantID()).Scan(&name, &price, &cost, &trackStock, &stock)
			if err != nil {
				return fmt.Errorf("product not found: %s", it.ProductID)
			}
			if trackStock && stock < it.Qty {
				return fmt.Errorf("out of stock: %s", name)
			}
			total += price * float64(it.Qty)
			// Sale item (tenant-scoped).
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO sports_kantin_sale_items (tenant_id, sale_id, product_id, product_name, qty, unit_price, unit_cost)
				 VALUES ($1,$2::uuid,$3::uuid,$4,$5,$6,$7)`,
				idn.TenantID(), saleID, it.ProductID, name, it.Qty, price, cost); err != nil {
				return err
			}
			if trackStock {
				if _, err := tx.Exec(r.Context(),
					`UPDATE sports_kantin_products SET stock = stock - $1 WHERE id=$2::uuid AND tenant_id=$3`,
					it.Qty, it.ProductID, idn.TenantID()); err != nil {
					return err
				}
				if _, err := tx.Exec(r.Context(),
					`INSERT INTO sports_kantin_stock_movements (tenant_id, product_id, delta, reason) VALUES ($1,$2::uuid,$3,'sale')`,
					idn.TenantID(), it.ProductID, -it.Qty); err != nil {
					return err
				}
			}
		}
		// Set the server-computed total.
		_, err := tx.Exec(r.Context(),
			`UPDATE sports_kantin_sales SET total=$1 WHERE id=$2::uuid`, total, saleID)
		return err
	})
	if err != nil {
		// Lost the insert race with a duplicate key: return the winner.
		if haveIdem && records.IsUniqueViolation(err) {
			var existingID string
			var existingTotal float64
			qerr := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
				return tx.QueryRow(r.Context(),
					`SELECT id::text, total FROM sports_kantin_sales WHERE tenant_id=$1 AND idempotency_key=$2`,
					idn.TenantID(), idemKey).Scan(&existingID, &existingTotal)
			})
			if qerr == nil {
				records.WriteJSON(w, http.StatusOK, map[string]any{"id": existingID, "total": existingTotal, "deduplicated": true})
				return
			}
		}
		msg := err.Error()
		if strings.Contains(msg, "out of stock") || strings.Contains(msg, "product not found") || msg == "bad qty" {
			records.WriteJSONError(w, http.StatusConflict, msg)
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusCreated, map[string]any{"id": saleID, "total": total})
}

type saleResp struct {
	ID           string  `json:"id"`
	TenantID     string  `json:"tenant_id"`
	Total        float64 `json:"total"`
	Method       string  `json:"method"`
	CustomerName string  `json:"customer_name"`
	Note         string  `json:"note"`
	CreatedAt    string  `json:"created_at"`
	// VoidedAt is what makes a voided sale visible in the history rather
	// than simply absent from it. It used to be deleted outright, so the
	// row that recorded a 60k correction had nothing left to live in.
	VoidedAt   string `json:"voided_at,omitempty"`
	VoidedBy   string `json:"voided_by,omitempty"`
	RecordedBy string `json:"recorded_by,omitempty"`
	// RecordedByName is joined in for the list so the history can say who
	// rang it up. The id alone is not something the owner can read.
	RecordedByName string `json:"recorded_by_name,omitempty"`
}

func (h *handler) listSales(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	// ?date= single WIB day, or ?from/?to range (to exclusive) on
	// created_at. ?limit bounds the rows (default 100, max 1000).
	q := r.URL.Query()
	date := strings.TrimSpace(q.Get("date"))
	from, to := strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
	for _, d := range []string{date, from, to} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				records.WriteJSONError(w, http.StatusBadRequest, "date/from/to must be YYYY-MM-DD")
				return
			}
		}
	}
	if from != "" && to != "" && to <= from {
		records.WriteJSONError(w, http.StatusBadRequest, "to must be after from")
		return
	}
	limit := 100
	if l := strings.TrimSpace(q.Get("limit")); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 1000 {
			n = 1000
		}
		limit = n
	}
	// recorded_by scopes the result to one staff member's own sales, for
	// the same reason as the payments list: the export route lets staff
	// see their own rows only, and "me" is resolved server-side so nobody
	// can pass another user's id.
	recordedBy := strings.TrimSpace(q.Get("recorded_by"))
	if recordedBy == "me" {
		recordedBy = idn.UserID
	}
	if recordedBy != "" {
		if _, err := uuid.Parse(recordedBy); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "recorded_by must be \"me\" or a uuid")
			return
		}
	}
	var out []saleResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Voided sales are listed, not hidden. They are excluded from money
		// arithmetic elsewhere; this screen exists precisely to show what
		// happened, so filtering them out here would hide the correction
		// the owner needs to see. The join is what turns recorded_by into
		// a name the owner can read.
		sql := `SELECT s.id::text, s.tenant_id, s.total, s.method, s.customer_name, s.note,
		               COALESCE(s.voided_at::text, ''), COALESCE(s.voided_by::text, ''),
COALESCE(s.recorded_by::text, ''), COALESCE(u.full_name, ''),
		               s.created_at
		          FROM sports_kantin_sales s
		          LEFT JOIN sports_users u ON u.id = s.recorded_by
		         WHERE s.tenant_id=$1`
		args := []any{idn.TenantID()}
		idx := 2
		// Qualified with s.: the recorder join below brings in a second table that
		// also has created_at and tenant_id, and an unqualified reference is
		// ambiguous -- the whole query failed with a 500 once it joined.
		wib := `(s.created_at AT TIME ZONE 'Asia/Jakarta')::date`
		if recordedBy != "" {
			sql += fmt.Sprintf(" AND s.recorded_by = $%d::uuid", idx)
			args = append(args, recordedBy)
			idx++
		}
		if date != "" {
			sql += fmt.Sprintf(" AND %s = $%d::date", wib, idx)
			args = append(args, date)
			idx++
		} else {
			if from != "" {
				sql += fmt.Sprintf(" AND %s >= $%d::date", wib, idx)
				args = append(args, from)
				idx++
			}
			if to != "" {
				sql += fmt.Sprintf(" AND %s < $%d::date", wib, idx)
				args = append(args, to)
				idx++
			}
		}
		sql += fmt.Sprintf(" ORDER BY s.created_at DESC LIMIT %d", limit)
		rows, err := tx.Query(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s saleResp
			var createdAt time.Time
			if err := rows.Scan(&s.ID, &s.TenantID, &s.Total, &s.Method, &s.CustomerName, &s.Note,
				&s.VoidedAt, &s.VoidedBy, &s.RecordedBy, &s.RecordedByName, &createdAt); err != nil {
				return err
			}
			s.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getSale(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var s saleResp
	var createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT s.id::text, s.tenant_id, s.total, s.method, s.customer_name, s.note,
			        COALESCE(s.voided_at::text, ''), COALESCE(s.voided_by::text, ''),
			        COALESCE(s.recorded_by::text, ''), COALESCE(u.full_name, ''),
			        s.created_at
			   FROM sports_kantin_sales s
			   LEFT JOIN sports_users u ON u.id = s.recorded_by
			  WHERE s.id=$1::uuid AND s.tenant_id=$2`,
			id, idn.TenantID(),
		).Scan(&s.ID, &s.TenantID, &s.Total, &s.Method, &s.CustomerName, &s.Note,
			&s.VoidedAt, &s.VoidedBy, &s.RecordedBy, &s.RecordedByName, &createdAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	s.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, s)
}

type saleItemResp struct {
	ID          string  `json:"id"`
	ProductName string  `json:"product_name"`
	Qty         int     `json:"qty"`
	UnitPrice   float64 `json:"unit_price"`
	UnitCost    float64 `json:"unit_cost"`
}

// errForbidden signals a cross-tenant access attempt inside a tx
// (mapped to 403 by the caller).
var errForbidden = errors.New("forbidden")

// listSaleItems returns the line items of one sale (riwayat detail +
// item-level export). Sale must belong to the caller's tenant.
func (h *handler) listSaleItems(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var out []saleItemResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Tenant check via the parent sale (items' tenant_id is nullable backfill).
		var saleTenant string
		if err := tx.QueryRow(r.Context(),
			`SELECT tenant_id FROM sports_kantin_sales WHERE id=$1::uuid`, id).Scan(&saleTenant); err != nil {
			return err
		}
		if saleTenant != idn.TenantID() {
			return errForbidden
		}
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, product_name, qty, unit_price, unit_cost
			   FROM sports_kantin_sale_items WHERE sale_id=$1::uuid ORDER BY product_name`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it saleItemResp
			if err := rows.Scan(&it.ID, &it.ProductName, &it.Qty, &it.UnitPrice, &it.UnitCost); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		if err == errForbidden {
			records.WriteJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if out == nil {
		out = []saleItemResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// voidSale atomically restores stock for tracked items, writes void
// movements, then deletes the sale (owner-only). Port of Centrum's
// void_kantin_sale RPC.
// Sentinels for the void preflight. They are compared outside the
// transaction closure, so they are values rather than the query error it
// would otherwise have to unwrap.
var (
	errAlreadyVoid = errors.New("kantin: sale already voided")
	errVoidLookup  = errors.New("kantin: void lookup failed")
	errSaleMissing = errors.New("kantin: sale not found")
)

func (h *handler) voidSale(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var voidErr error
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Lock the sale and prove it is still live.
		//
		// This is what makes a repeat void safe. The row used to be deleted,
		// and the handler never checked whether one existed: the item query
		// returned nothing for a second void, so no stock was restored
		// again -- by luck, because there was nothing left to restore from.
		// With a soft void the row survives, so without this check a double
		// click would restore the stock a second time and write a second
		// set of movements. Refusing a sale that is already voided is also
		// the honest answer: the owner asked to void something that is not
		// live, and there is nothing to do.
		//
		// FOR UPDATE serialises two concurrent voids of the same sale, so
		// the second waits and then sees the first's work.
		var live bool
		if err := tx.QueryRow(r.Context(),
			`SELECT voided_at IS NULL FROM sports_kantin_sales
			  WHERE id=$1::uuid AND tenant_id=$2 FOR UPDATE`,
			id, idn.TenantID()).Scan(&live); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				voidErr = errSaleMissing
			}
			return errVoidLookup
		}
		if !live {
			voidErr = errAlreadyVoid
			return errVoidLookup
		}

		// Restore stock only for products that actually track it.
		//
		// The restore loop was unconditional, so voiding a sale containing an
		// untracked item (water, say -- a drink with no stock record) added
		// a phantom unit to that product's stock. The sale reduced stock
		// through track_stock too, so this only looked balanced for tracked
		// items and quietly invented inventory for everything else.
		rows, err := tx.Query(r.Context(),
			`SELECT si.product_id::text, si.qty
			   FROM sports_kantin_sale_items si
			   JOIN sports_kantin_products p ON p.id = si.product_id AND p.tenant_id = si.tenant_id
			  WHERE si.sale_id=$1::uuid AND si.product_id IS NOT NULL AND p.track_stock`,
			id)
		if err != nil {
			return err
		}
		type item struct {
			productID string
			qty       int
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.productID, &it.qty); err != nil {
				rows.Close()
				return err
			}
			items = append(items, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Restore stock + write void movements.
		for _, it := range items {
			if _, err := tx.Exec(r.Context(),
				`UPDATE sports_kantin_products SET stock = stock + $1 WHERE id=$2::uuid AND tenant_id=$3`,
				it.qty, it.productID, idn.TenantID()); err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO sports_kantin_stock_movements (tenant_id, product_id, delta, reason, note)
				 VALUES ($1,$2::uuid,$3,'void', 'void sale')`,
				idn.TenantID(), it.productID, it.qty); err != nil {
				return err
			}
		}
		// Mark it voided. The row and its line items stay: a sale is money,
		// and deleting it destroys the only record of what was rung up.
		// Queries that count revenue filter on voided_at.
		var createdByArg any
		if idn.UserID != "" {
			createdByArg = idn.UserID
		}
		if _, err := tx.Exec(r.Context(),
			`UPDATE sports_kantin_sales SET voided_at=now(), voided_by=$3 WHERE id=$1::uuid AND tenant_id=$2`,
			id, idn.TenantID(), createdByArg); err != nil {
			return err
		}
		return nil
	})
	if voidErr != nil {
		if errors.Is(voidErr, errAlreadyVoid) {
			records.WriteJSONError(w, http.StatusConflict, "sale is already voided")
			return
		}
		records.WriteJSONError(w, http.StatusNotFound, "sale not found")
		return
	}
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "voided"})
}
