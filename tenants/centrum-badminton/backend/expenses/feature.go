// Package expenses implements the "expenses" feature: owner expense
// ledger (categories, notes). Port of CentrumBadminton's expenses.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package expenses

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

const featureName = "expenses"

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
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", records.WithValidID(h.getOne))
	r.Patch("/{id}", records.WithValidID(h.patch))
	r.Delete("/{id}", records.WithValidID(h.del))
}

type handler struct {
	pool *pgxpool.Pool
}

type expenseResp struct {
	ID        string  `json:"id"`
	TenantID  string  `json:"tenant_id"`
	Category  string  `json:"category"`
	Amount    float64 `json:"amount"`
	Note      string  `json:"note"`
	SpentAt   string  `json:"spent_at"`
	CreatedAt string  `json:"created_at"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	var req struct {
		Category string  `json:"category"`
		Amount   float64 `json:"amount"`
		Note     string  `json:"note"`
		SpentAt  string  `json:"spent_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Amount <= 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "amount must be > 0")
		return
	}
	cat := req.Category
	if cat == "" {
		cat = "lainnya"
	}
	spentAt := req.SpentAt
	if spentAt == "" {
		// WIB, not the server's local time: a UTC host would file the
		// expense under yesterday for seven hours a day, and every
		// summary query in this file filters in Asia/Jakarta.
		spentAt = time.Now().In(time.FixedZone("WIB", 7*3600)).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", spentAt); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "spent_at must be YYYY-MM-DD")
		return
	}
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_expenses (tenant_id, category, amount, note, spent_at)
			 VALUES ($1,$2,$3,$4,$5::date) RETURNING id::text`,
			idn.TenantID(), cat, req.Amount, req.Note, spentAt).Scan(&id)
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
	// spent_at is a DATE column: ?date= single day, or ?from/?to range
	// (to exclusive). ?limit bounds the rows (default 500, max 5000).
	q := r.URL.Query()
	dateStr, from, to := strings.TrimSpace(q.Get("date")), strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
	for _, d := range []string{dateStr, from, to} {
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
	limit := 500
	if l := strings.TrimSpace(q.Get("limit")); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 5000 {
			n = 5000
		}
		limit = n
	}
	var out []expenseResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		sql := `SELECT id::text, tenant_id, category, amount, note, spent_at::text, created_at
			   FROM sports_expenses WHERE tenant_id=$1 AND deleted_at IS NULL`
		args := []any{idn.TenantID()}
		idx := 2
		if dateStr != "" {
			sql += fmt.Sprintf(" AND spent_at = $%d::date", idx)
			args = append(args, dateStr)
			idx++
		} else {
			if from != "" {
				sql += fmt.Sprintf(" AND spent_at >= $%d::date", idx)
				args = append(args, from)
				idx++
			}
			if to != "" {
				sql += fmt.Sprintf(" AND spent_at < $%d::date", idx)
				args = append(args, to)
				idx++
			}
		}
		sql += fmt.Sprintf(" ORDER BY spent_at DESC, created_at DESC LIMIT %d", limit)
		rows, err := tx.Query(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e expenseResp
			var createdAt time.Time
			if err := rows.Scan(&e.ID, &e.TenantID, &e.Category, &e.Amount, &e.Note, &e.SpentAt, &createdAt); err != nil {
				return err
			}
			e.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, e)
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
	var e expenseResp
	var createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, category, amount, note, spent_at::text, created_at
			   FROM sports_expenses WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
			id, idn.TenantID(),
		).Scan(&e.ID, &e.TenantID, &e.Category, &e.Amount, &e.Note, &e.SpentAt, &createdAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	e.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, e)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Category *string  `json:"category"`
		Amount   *float64 `json:"amount"`
		Note     *string  `json:"note"`
		SpentAt  *string  `json:"spent_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{}
	args := []any{}
	idx := 1
	if req.Category != nil {
		sets = append(sets, "category = $"+strconv.Itoa(idx))
		args = append(args, *req.Category)
		idx++
	}
	if req.Amount != nil {
		if *req.Amount <= 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "amount must be > 0")
			return
		}
		sets = append(sets, "amount = $"+strconv.Itoa(idx))
		args = append(args, *req.Amount)
		idx++
	}
	if req.Note != nil {
		sets = append(sets, "note = $"+strconv.Itoa(idx))
		args = append(args, *req.Note)
		idx++
	}
	if req.SpentAt != nil {
		if _, err := time.Parse("2006-01-02", *req.SpentAt); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "spent_at must be YYYY-MM-DD")
			return
		}
		sets = append(sets, "spent_at = $"+strconv.Itoa(idx)+"::date")
		args = append(args, *req.SpentAt)
		idx++
	}
	if len(sets) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_expenses SET %s WHERE id=$%d::uuid AND tenant_id=$%d AND deleted_at IS NULL",
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
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Tombstone, not DELETE: the ledger row is preserved with who/when,
		// hidden from lists/reads/totals.
		deletedBy := idn.UserID
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_expenses SET deleted_at = now(), deleted_by = $3
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
