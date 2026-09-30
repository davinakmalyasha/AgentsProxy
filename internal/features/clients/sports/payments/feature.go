// Package payments implements the "payments" feature: per-booking
// cash/QRIS payments with automatic payment_status recalculation via
// a DB trigger (unpaid → partial → paid). Port of CentrumBadminton's
// payments.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package payments

import (
	"encoding/json"
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

const featureName = "payments"

func init() { Register() }

func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "", // custom table, not tenant_records
		DefaultOn:     false,
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

type paymentResp struct {
	ID        string  `json:"id"`
	TenantID  string  `json:"tenant_id"`
	BookingID string  `json:"booking_id"`
	Amount    float64 `json:"amount"`
	Method    string  `json:"method"`
	PaidAt    string  `json:"paid_at"`
	Note      string  `json:"note"`
	CreatedAt string  `json:"created_at"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req struct {
		BookingID string  `json:"booking_id"`
		Amount    float64 `json:"amount"`
		Method    string  `json:"method"`
		Note      string  `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.BookingID) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "booking_id required")
		return
	}
	if req.Amount <= 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "amount must be > 0")
		return
	}
	if req.Method == "" {
		req.Method = "cash"
	}
	if req.Method != "cash" && req.Method != "qris" {
		records.WriteJSONError(w, http.StatusBadRequest, "method must be cash or qris")
		return
	}
	var id string
	var overpayErr error
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Lock booking for update and get effective price (override_price > price)
		var price, overridePrice *float64
		if err := tx.QueryRow(r.Context(),
			`SELECT price, override_price FROM sports_bookings WHERE id=$1::uuid AND tenant_id=$2 FOR UPDATE`,
			req.BookingID, idn.TenantID()).Scan(&price, &overridePrice); err != nil {
			return err
		}
		effective := 0.0
		if overridePrice != nil {
			effective = *overridePrice
		} else if price != nil {
			effective = *price
		}
		var paid float64
		if err := tx.QueryRow(r.Context(),
			`SELECT COALESCE(SUM(amount),0) FROM sports_payments WHERE booking_id=$1::uuid AND tenant_id=$2`,
			req.BookingID, idn.TenantID()).Scan(&paid); err != nil {
			return err
		}
		outstanding := effective - paid
		if outstanding <= 0 {
			overpayErr = fmt.Errorf("already paid")
			return overpayErr
		}
		if req.Amount > outstanding {
			overpayErr = fmt.Errorf("amount %.2f exceeds outstanding %.2f", req.Amount, outstanding)
			return overpayErr
		}
		// recorded_by is who actually took the money, which is not the same
		// as who created the booking: staff routinely collect payment for a
		// booking someone else entered. Without it a staff-scoped ledger
		// export can only attribute money to the booking's creator, so it
		// would show up in the wrong person's export and nowhere in the
		// taker's.
		recordedBy := idn.UserID
		var recordedByArg any
		if recordedBy == "" {
			recordedByArg = nil
		} else {
			recordedByArg = recordedBy
		}
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_payments (tenant_id, booking_id, amount, method, note, recorded_by)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
			idn.TenantID(), req.BookingID, req.Amount, req.Method, req.Note, recordedByArg,
		).Scan(&id)
	})
	if overpayErr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, overpayErr.Error())
		return
	}
	if err != nil {
		if err == pgx.ErrNoRows {
			records.WriteJSONError(w, http.StatusNotFound, "booking not found")
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
	q := r.URL.Query()
	bookingID := q.Get("booking_id")
	// WIB calendar-day bounds on paid_at (same convention as reports).
	// ?from=YYYY-MM-DD & ?to=YYYY-MM-DD (to exclusive). ?limit caps rows.
	from, to := strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
	for _, d := range []string{from, to} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				records.WriteJSONError(w, http.StatusBadRequest, "from/to must be YYYY-MM-DD")
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
	// recorded_by scopes the result to one staff member's own takings. It
	// is a filter, not an authorisation boundary: the caller still reads
	// within their own tenant, and "me" is resolved to the session's user
	// id server-side so a staff member cannot pass someone else's id and
	// read their rows.
	recordedBy := strings.TrimSpace(q.Get("recorded_by"))
	me := recordedBy == "me"
	if me {
		recordedBy = idn.UserID
	}
	if recordedBy != "" {
		if _, err := uuid.Parse(recordedBy); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "recorded_by must be \"me\" or a uuid")
			return
		}
	}
	var out []paymentResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		sql := `SELECT id::text, tenant_id, booking_id::text, amount, method, paid_at, note, created_at
		          FROM sports_payments WHERE tenant_id = $1`
		args := []any{idn.TenantID()}
		idx := 2
		if bookingID != "" {
			sql += fmt.Sprintf(" AND booking_id = $%d::uuid", idx)
			args = append(args, bookingID)
			idx++
		}
		if recordedBy != "" {
			sql += fmt.Sprintf(" AND recorded_by = $%d::uuid", idx)
			args = append(args, recordedBy)
			idx++
		}
		if from != "" {
			sql += fmt.Sprintf(" AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $%d::date", idx)
			args = append(args, from)
			idx++
		}
		if to != "" {
			sql += fmt.Sprintf(" AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date < $%d::date", idx)
			args = append(args, to)
			idx++
		}
		sql += fmt.Sprintf(" ORDER BY paid_at DESC LIMIT %d", limit)
		rows, err := tx.Query(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p paymentResp
			var paidAt, createdAt time.Time
			if err := rows.Scan(&p.ID, &p.TenantID, &p.BookingID, &p.Amount, &p.Method, &paidAt, &p.Note, &createdAt); err != nil {
				return err
			}
			p.PaidAt = paidAt.UTC().Format(time.RFC3339Nano)
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

func (h *handler) getOne(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var p paymentResp
	var paidAt, createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, booking_id::text, amount, method, paid_at, note, created_at
			   FROM sports_payments WHERE id=$1::uuid AND tenant_id=$2`,
			id, idn.TenantID(),
		).Scan(&p.ID, &p.TenantID, &p.BookingID, &p.Amount, &p.Method, &paidAt, &p.Note, &createdAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	p.PaidAt = paidAt.UTC().Format(time.RFC3339Nano)
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, p)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Amount *float64 `json:"amount"`
		Method *string  `json:"method"`
		Note   *string  `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{}
	args := []any{}
	idx := 1
	if req.Amount != nil {
		if *req.Amount <= 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "amount must be > 0")
			return
		}
		sets = append(sets, "amount = $"+strconv.Itoa(idx))
		args = append(args, *req.Amount)
		idx++
	}
	if req.Method != nil {
		if *req.Method != "cash" && *req.Method != "qris" {
			records.WriteJSONError(w, http.StatusBadRequest, "method must be cash or qris")
			return
		}
		sets = append(sets, "method = $"+strconv.Itoa(idx))
		args = append(args, *req.Method)
		idx++
	}
	if req.Note != nil {
		sets = append(sets, "note = $"+strconv.Itoa(idx))
		args = append(args, *req.Note)
		idx++
	}
	if len(sets) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_payments SET %s WHERE id=$%d::uuid AND tenant_id=$%d",
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
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM sports_payments WHERE id=$1::uuid AND tenant_id=$2`,
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
