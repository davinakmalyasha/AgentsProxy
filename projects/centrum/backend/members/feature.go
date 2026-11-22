// Package members implements the sports-vertical "members" feature:
// regulars with a prepaid balance. Uses a custom table
// (sports_members) because a prepaid balance needs a NUMERIC column
// with atomic top-ups.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package members

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

const featureName = "members"

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
	r.Post("/{id}/topup", records.WithValidID(h.topup))
}

type handler struct {
	pool *pgxpool.Pool
}

type memberResp struct {
	ID        string  `json:"id"`
	TenantID  string  `json:"tenant_id"`
	Name      string  `json:"name"`
	Phone     string  `json:"phone"`
	Balance   float64 `json:"balance"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req struct {
		Name    string  `json:"name"`
		Phone   string  `json:"phone"`
		Balance float64 `json:"balance"`
		Status  string  `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	if req.Balance < 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "balance must be >= 0")
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
			`INSERT INTO sports_members (tenant_id, name, phone, balance, status)
			 VALUES ($1,$2,$3,$4,$5) RETURNING id::text`,
			idn.TenantID(), req.Name, req.Phone, req.Balance, status,
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
	out := make([]memberResp, 0, limit+1)
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, tenant_id, name, phone, balance, status, created_at, updated_at
			   FROM sports_members WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY name LIMIT $2`,
			idn.TenantID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m memberResp
			var createdAt, updatedAt time.Time
			if err := rows.Scan(&m.ID, &m.TenantID, &m.Name, &m.Phone, &m.Balance, &m.Status,
				&createdAt, &updatedAt); err != nil {
				return err
			}
			m.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			m.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if out == nil {
		out = []memberResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getOne(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var m memberResp
	var createdAt, updatedAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, name, phone, balance, status, created_at, updated_at
			   FROM sports_members WHERE id = $1::uuid AND tenant_id = $2 AND deleted_at IS NULL`,
			id, idn.TenantID(),
		).Scan(&m.ID, &m.TenantID, &m.Name, &m.Phone, &m.Balance, &m.Status, &createdAt, &updatedAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	m.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	m.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, m)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Name   *string `json:"name"`
		Phone  *string `json:"phone"`
		Status *string `json:"status"`
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
	if req.Phone != nil {
		sets = append(sets, "phone = $"+strconv.Itoa(idx))
		args = append(args, *req.Phone)
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
	sql := fmt.Sprintf("UPDATE sports_members SET %s WHERE id = $%d::uuid AND tenant_id = $%d AND deleted_at IS NULL",
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
		// Tombstone, not DELETE: a member row carries balance. Only the
		// owner removes, and the row is preserved with who/when.
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_members SET deleted_at = now(), deleted_by = $3
			  WHERE id = $1::uuid AND tenant_id = $2 AND deleted_at IS NULL`,
			id, idn.TenantID(), idn.UserID)
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

// topup atomically adds to a member's balance. Positive = top-up.
// Balance never goes negative.
func (h *handler) topup(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Amount <= 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "amount must be > 0")
		return
	}
	// Idempotency-Key (optional phase): a repeated key returns the original
	// topup without crediting twice. The key lives on a ledger row because
	// the topup itself is an UPDATE. Absent header behaves as before.
	idemKey, haveIdem, idemErr := records.IdempotencyKey(w, r)
	if idemErr != nil {
		return
	}
	// Single transaction: dedup-check, ledger insert, and balance update are
	// one atomic unit. The pre-read used to live in its own transaction,
	// so a concurrent duplicate could slip between check and write; now
	// the partial unique index (tenant_id, idempotency_key, member_id) is
	// the arbiter and the fallback below returns the winner. This also
	// keeps the closure side-effect-free for the deadlock retry: re-running
	// re-reads instead of double-firing.
	var newBalance float64
	deduplicated := false
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		if haveIdem {
			var bal float64
			qerr := tx.QueryRow(r.Context(),
				`SELECT m.balance FROM sports_member_topups t
				  JOIN sports_members m ON m.id = t.member_id AND m.tenant_id = t.tenant_id AND m.deleted_at IS NULL
				  WHERE t.tenant_id=$1 AND t.idempotency_key=$2 AND t.member_id=$3::uuid`,
				idn.TenantID(), idemKey, id).Scan(&bal)
			if qerr == nil {
				newBalance, deduplicated = bal, true
				return nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return qerr
			}
		}
		// Update first: an id that does not belong to this tenant must be
		// a clean 404, not the FK violation the ledger insert would raise.
		// The row lock also makes the ledger write safe against a
		// concurrent member delete.
		err := tx.QueryRow(r.Context(),
			`UPDATE sports_members SET balance = balance + $1, updated_at = now()
			  WHERE id = $2::uuid AND tenant_id = $3 AND deleted_at IS NULL
			  RETURNING balance`,
			req.Amount, id, idn.TenantID(),
		).Scan(&newBalance)
		if err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(),
			`INSERT INTO sports_member_topups (tenant_id, member_id, amount, idempotency_key)
			 VALUES ($1,$2::uuid,$3,NULLIF($4,''))`,
			idn.TenantID(), id, req.Amount, idemKey)
		return err
	})
	if err != nil {
		// Lost the ledger race with a duplicate key: return the winner.
		if haveIdem && records.IsUniqueViolation(err) {
			var bal float64
			qerr := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
				return tx.QueryRow(r.Context(),
					`SELECT m.balance FROM sports_member_topups t
					  JOIN sports_members m ON m.id = t.member_id AND m.tenant_id = t.tenant_id AND m.deleted_at IS NULL
					  WHERE t.tenant_id=$1 AND t.idempotency_key=$2 AND t.member_id=$3::uuid`,
					idn.TenantID(), idemKey, id).Scan(&bal)
			})
			if qerr == nil {
				records.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "balance": bal, "deduplicated": true})
				return
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			records.WriteJSONError(w, http.StatusNotFound, "not found")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if deduplicated {
		records.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "balance": newBalance, "deduplicated": true})
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "balance": newBalance})
}
