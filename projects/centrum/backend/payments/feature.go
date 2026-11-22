// Package payments implements the "payments" feature: per-booking
// cash/QRIS payments with automatic payment_status recalculation via
// a DB trigger (unpaid → partial → paid). Port of CentrumBadminton's
// payments.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package payments

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

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "payments"

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
	if _, err := uuid.Parse(strings.TrimSpace(req.BookingID)); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "booking_id must be a uuid")
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
	// Idempotency-Key (optional phase): a repeated key returns the original
	// payment instead of inserting a second charge. Absent header behaves
	// exactly as before.
	idemKey, haveIdem, idemErr := records.IdempotencyKey(w, r)
	if idemErr != nil {
		return
	}
	if haveIdem {
		var existing string
		qerr := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT id::text FROM sports_payments WHERE tenant_id=$1 AND idempotency_key=$2`,
				idn.TenantID(), idemKey).Scan(&existing)
		})
		if qerr == nil {
			records.WriteJSON(w, http.StatusOK, map[string]any{"id": existing, "deduplicated": true})
			return
		}
		if qerr != pgx.ErrNoRows {
			records.WriteJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
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
			`SELECT COALESCE(SUM(amount),0) FROM sports_payments WHERE booking_id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
			req.BookingID, idn.TenantID()).Scan(&paid); err != nil {
			return err
		}
		outstanding := effective - paid
		if outstanding <= 0 {
			overpayErr = fmt.Errorf("already paid")
			return overpayErr
		}
		if req.Amount > outstanding {
			overpayErr = fmt.Errorf("amount %s exceeds outstanding %s",
				agentstools.Idr(req.Amount), agentstools.Idr(outstanding))
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
			`INSERT INTO sports_payments (tenant_id, booking_id, amount, method, note, recorded_by, idempotency_key)
			 VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,'')) RETURNING id::text`,
			idn.TenantID(), req.BookingID, req.Amount, req.Method, req.Note, recordedByArg, idemKey,
		).Scan(&id)
	})
	if overpayErr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, overpayErr.Error())
		return
	}
	if err != nil {
		// Lost the insert race with a duplicate key: return the winner.
		if haveIdem && records.IsUniqueViolation(err) {
			var existing string
			qerr := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
				return tx.QueryRow(r.Context(),
					`SELECT id::text FROM sports_payments WHERE tenant_id=$1 AND idempotency_key=$2`,
					idn.TenantID(), idemKey).Scan(&existing)
			})
			if qerr == nil {
				records.WriteJSON(w, http.StatusOK, map[string]any{"id": existing, "deduplicated": true})
				return
			}
		}
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
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	q := r.URL.Query()
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
	out := make([]paymentResp, 0, limit+1)
	truncated := false
	bookingID := strings.TrimSpace(q.Get("booking_id"))
	if bookingID != "" {
		if _, err := uuid.Parse(bookingID); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "booking_id must be a uuid")
			return
		}
	}
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		sql := `SELECT id::text, tenant_id, booking_id::text, amount, method, paid_at, note, created_at
		          FROM sports_payments WHERE tenant_id = $1 AND deleted_at IS NULL`
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
		// One row past the cap detects truncation without a second COUNT.
		sql += fmt.Sprintf(" ORDER BY paid_at DESC LIMIT %d", limit+1)
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
	// `truncated` is reported so the Laporan screen and the export can say
	// a total is partial instead of presenting a capped sum as final. This
	// list is the revenue ledger, so a silently shortened one is a wrong
	// number that looks right.
	if out == nil {
		out = []paymentResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "truncated": truncated})
}

func (h *handler) getOne(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var p paymentResp
	var paidAt, createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, booking_id::text, amount, method, paid_at, note, created_at
			   FROM sports_payments WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
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
	idn := records.RequireOwner(w, r)
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
	sql := fmt.Sprintf("UPDATE sports_payments SET %s WHERE id=$%d::uuid AND tenant_id=$%d AND deleted_at IS NULL",
		strings.Join(sets, ", "), idx, idx+1)
	var affected int64
	var overpayErr error
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		if req.Amount != nil {
			// Re-check the outstanding balance, exactly as create does.
			//
			// create refuses an amount above the outstanding, but patch
			// only checked amount > 0. So a recorded payment could be
			// edited upwards until the ledger said more was collected than
			// the booking is worth. The recalc trigger then set
			// payment_status='paid' while the sums disagreed, and the
			// reports' revenue no longer matched what the booking should
			// have cost.
			//
			// The row is locked FOR UPDATE so a concurrent create cannot
			// slip a payment in between this check and the update.
			// The payment's own row tells us which booking it settles; the
			// booking supplies the amount owed.
			var bookingID string
			if err := tx.QueryRow(r.Context(),
				`SELECT booking_id::text FROM sports_payments
				  WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL FOR UPDATE`,
				id, idn.TenantID()).Scan(&bookingID); err != nil {
				return err
			}
			var price, overridePrice *float64
			if err := tx.QueryRow(r.Context(),
				`SELECT price, override_price FROM sports_bookings
				  WHERE id=$1::uuid AND tenant_id=$2 FOR UPDATE`,
				bookingID, idn.TenantID()).Scan(&price, &overridePrice); err != nil {
				return err
			}
			var effective float64
			if overridePrice != nil {
				effective = *overridePrice
			} else if price != nil {
				effective = *price
			}
			var paid float64
			if err := tx.QueryRow(r.Context(),
				`SELECT COALESCE(SUM(amount),0) FROM sports_payments
				  WHERE booking_id=$1::uuid AND tenant_id=$2 AND id<>$3::uuid AND deleted_at IS NULL`,
				bookingID, idn.TenantID(), id).Scan(&paid); err != nil {
				return err
			}
			if newTotal := paid + *req.Amount; newTotal > effective+1e-9 {
				overpayErr = fmt.Errorf("amount %s exceeds outstanding %s",
					agentstools.Idr(*req.Amount), agentstools.Idr(effective-paid))
				return overpayErr
			}
		}
		tag, err := tx.Exec(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if overpayErr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, overpayErr.Error())
		return
	}
	if err != nil {
		// Missing payment (or its booking vanished mid-edit) is a 404,
		// not a 500: the inner SELECTs surface pgx.ErrNoRows.
		if errors.Is(err, pgx.ErrNoRows) {
			records.WriteJSONError(w, http.StatusNotFound, "not found")
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
		// Tombstone, not DELETE: the ledger row is preserved with who/when,
		// hidden from lists/reads/totals. The UPDATE fires the recalc
		// trigger, so the booking's payment_status drops back by itself.
		// The idempotency key is freed so a corrected re-payment can reuse
		// the client's key.
		deletedBy := idn.UserID
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_payments SET deleted_at = now(), deleted_by = $3, idempotency_key = NULL
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
