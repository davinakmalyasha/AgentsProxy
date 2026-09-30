// Package bookings implements the sports-vertical "bookings"
// feature. Uses a custom table (sports_bookings) with an exclusion
// constraint that makes double-booking IMPOSSIBLE at the database
// level — no application code can accidentally overlap a facility's
// time range, no matter how concurrent the requests.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package bookings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clients/sports/pricing"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "bookings"

func init() { Register() }

// NotifyFunc is called after a booking is created/updated. The
// sports feature wires this to the notify queue in main.go; tests
// can leave it nil. The tenant is passed explicitly because the
// enqueue happens outside this request's transaction, so there is no
// ambient tenant context to read.
var NotifyFunc func(tenantID, to, text string)

// Register registers the bookings feature. Safe to call from tests
// after features.ResetForTest().
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

type bookingReq struct {
	FacilityID    string  `json:"facility_id"`
	Date          string  `json:"date"` // YYYY-MM-DD
	StartTime     string  `json:"start_time"`
	EndTime       string  `json:"end_time"`
	CustomerName  string  `json:"customer_name"`
	CustomerPhone string  `json:"customer_phone"`
	Price         float64 `json:"price"` // 0 → computed from pricing rule
	Deposit       float64 `json:"deposit"`
	Status        string  `json:"status"`
	Source        string  `json:"source"` // whatsapp | walk_in
	Notes         string  `json:"notes"`
}

type bookingResp struct {
	ID            string   `json:"id"`
	TenantID      string   `json:"tenant_id"`
	FacilityID    string   `json:"facility_id"`
	FacilityName  string   `json:"facility_name,omitempty"`
	Date          string   `json:"date"`
	StartTime     string   `json:"start_time"`
	EndTime       string   `json:"end_time"`
	CustomerName  string   `json:"customer_name"`
	CustomerPhone string   `json:"customer_phone"`
	Status        string   `json:"status"`
	Source        string   `json:"source"`
	Price         float64  `json:"price"`
	OverridePrice *float64 `json:"override_price,omitempty"`
	PaymentStatus string   `json:"payment_status"`
	Deposit       float64  `json:"deposit"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

var validBookingStatuses = map[string]bool{
	"pending": true, "confirmed": true, "checked_in": true,
	"completed": true, "cancelled": true, "no_show": true,
}

// validTransitions is the authoritative state machine for a booking.
// Hoisted to package scope so the PATCH handler and cancellableFrom
// below are driven by the same table — previously it was a local, which
// is how DELETE ended up able to cancel states PATCH would refuse.
var validTransitions = map[string][]string{
	"pending":    {"confirmed", "cancelled"},
	"confirmed":  {"checked_in", "cancelled", "no_show"},
	"checked_in": {"completed", "no_show"},
}

// buildUpdateSQL assembles the booking UPDATE for the PATCH handler.
//
// It is a function rather than an inline fmt.Sprintf so the compare-and-set
// predicate can be asserted without a database. `idArg` is the position of
// the id parameter; `guard` adds the status predicate at the position
// after it and reports how many extra args the caller must append.
//
// The guard is the whole point: the handler reads the current status to
// validate the transition, then writes. Building the statement before the
// read left a window in which a second writer could move the row, and this
// UPDATE matched on id alone — so the write landed on a state nobody
// validated. `AND status = $n` makes it one atomic compare-and-set: if
// anyone else got there first, zero rows match and the caller reports a
// conflict instead of silently overwriting.
func buildUpdateSQL(sets []string, idArg int, guard bool) (sql string, extraArgs int) {
	sql = fmt.Sprintf("UPDATE sports_bookings SET %s WHERE id = $%d::uuid AND tenant_id = $%d",
		strings.Join(sets, ", "), idArg, idArg+1)
	if guard {
		sql += fmt.Sprintf(" AND status = $%d", idArg+2)
		return sql, 1
	}
	return sql, 0
}

// errOutsideHours is returned when a booking falls outside the
// facility's open/close window; mapped to 400 (not 500).
var errOutsideHours = errors.New("booking outside facility hours")


// errNotCancellable is returned when a cancel is attempted from a state
// that is not cancellable. Mapped to 409 so the UI can say "already
// completed" instead of a generic failure.
var errNotCancellable = errors.New("booking is not in a cancellable state")

// cancellableFrom is the set of states a cancel may be applied from.
//
// It is derived from validTransitions rather than restated, so DELETE
// and the PATCH transition rule can never disagree about what "cancel"
// means. pending and confirmed have a 'cancelled' target; completed and
// cancelled are terminal; checked_in and no_show describe a slot that
// was really used, so cancelling them would erase a real court event
// and re-open an occupied slot.
var cancellableFrom = buildCancellableFrom()

func buildCancellableFrom() map[string]bool {
	m := map[string]bool{}
	for from, targets := range validTransitions {
		for _, to := range targets {
			if to == "cancelled" {
				m[from] = true
			}
		}
	}
	return m
}

// cancellableFromSQL is the same set as a SQL literal, for the DELETE
// predicate. Sorted so the generated SQL is stable and diffable.
var cancellableFromSQL = func() string {
	allowed := make([]string, 0, len(cancellableFrom))
	for s := range cancellableFrom {
		allowed = append(allowed, s)
	}
	sort.Strings(allowed)
	return "'" + strings.Join(allowed, "','") + "'"
}()

// create validates and inserts a booking. The exclusion constraint
// rejects overlaps; we map that to a clean 409.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req bookingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.FacilityID) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "facility_id required")
		return
	}
	date, err := time.Parse("2006-01-02", req.Date)
	if err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	start, err := time.Parse("15:04", req.StartTime)
	if err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "start_time must be HH:MM")
		return
	}
	end, err := time.Parse("15:04", req.EndTime)
	if err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "end_time must be HH:MM")
		return
	}
	if !end.After(start) {
		records.WriteJSONError(w, http.StatusBadRequest, "end_time must be after start_time")
		return
	}
	if strings.TrimSpace(req.CustomerName) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "customer_name required")
		return
	}
	status := req.Status
	if status == "" {
		status = "confirmed"
	}
	if !validBookingStatuses[status] {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
		return
	}
	source := req.Source
	if source == "" {
		source = "whatsapp"
	}
	if source != "whatsapp" && source != "walk_in" {
		records.WriteJSONError(w, http.StatusBadRequest, "source must be whatsapp or walk_in")
		return
	}
	if req.Price < 0 || req.Deposit < 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "price/deposit must be >= 0")
		return
	}

	var (
		id       string
		facility string
		openStr  string
		closeStr string
	)
	err = records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Validate the facility exists + is active + within open hours.
		err := tx.QueryRow(r.Context(),
			`SELECT name, open_time::text, close_time::text FROM sports_facilities
			  WHERE id = $1::uuid AND tenant_id = $2 AND status = 'active'`,
			req.FacilityID, idn.TenantID(),
		).Scan(&facility, &openStr, &closeStr)
		if err != nil {
			return err // no rows → facility not found
		}
		open, errO := time.Parse("15:04", strings.TrimSuffix(openStr, ":00"))
		closeT, errC := time.Parse("15:04", strings.TrimSuffix(closeStr, ":00"))
		if errO != nil || errC != nil {
			return fmt.Errorf("facility hours unparseable: %s-%s", openStr, closeStr)
		}
		if start.Before(open) || end.After(closeT) {
			return fmt.Errorf("%w: %s-%s", errOutsideHours, openStr, closeStr)
		}
		price := req.Price
		// Non-owners cannot trust client price — always server-compute
		if !idn.IsOwner() || price == 0 {
			if computed, err := pricing.PriceFor(r.Context(), tx, idn.TenantID(), date, start, end); err == nil {
				price = computed
			} else if !idn.IsOwner() {
				price = 0 // fallback, akan diisi trigger? keep 0 for now
			}
		}
		// Link a customer by phone (Centrum's upsert pattern) when the
		// phone matches an existing directory entry.
		var customerID *string
		if req.CustomerPhone != "" {
			var cid string
			if err := tx.QueryRow(r.Context(),
				`SELECT id::text FROM sports_customers WHERE tenant_id=$1 AND phone=$2`,
				idn.TenantID(), req.CustomerPhone).Scan(&cid); err == nil {
				customerID = &cid
			}
		}
		createdBy := idn.UserID
		var createdByArg any
		if createdBy == "" {
			createdByArg = nil
		} else {
			createdByArg = createdBy
		}
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_bookings
			   (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status, source, price, deposit, notes, customer_id, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id::text`,
			idn.TenantID(), req.FacilityID, date, start, end, req.CustomerName, req.CustomerPhone, status, source, price, req.Deposit,
			req.Notes, customerID, createdByArg,
		).Scan(&id)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			records.WriteJSONError(w, http.StatusNotFound, "facility not found")
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
			// exclusion_violation → the slot is taken
			records.WriteJSONError(w, http.StatusConflict, "slot already booked")
			return
		}
		if errors.Is(err, errOutsideHours) {
			records.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("booking create", "err", err)
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if NotifyFunc != nil && status == "confirmed" {
		NotifyFunc(idn.TenantID(), req.CustomerPhone, fmt.Sprintf(
			"Booking confirmed: %s at %s, %s %s-%s",
			req.CustomerName, facility, date.Format("Mon Jan 2"), req.StartTime, req.EndTime))
	}
	records.WriteJSON(w, http.StatusCreated, map[string]string{"id": id, "status": status})
}

// list returns bookings for a date and/or facility (the daily
// schedule view). Status filter: ?status=a,b (default confirmed for
// backward compat), plus repeatable ?status.neq= to exclude. ?limit
// bounds the rows (default 500, max 2000).
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	q := r.URL.Query()
	dateStr := q.Get("date")
	facilityID := q.Get("facility_id")
	statuses := []string{"confirmed"}
	if s := strings.TrimSpace(q.Get("status")); s != "" {
		statuses = nil
		for _, part := range strings.Split(s, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if !validBookingStatuses[part] {
				records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
				return
			}
			statuses = append(statuses, part)
		}
		if len(statuses) == 0 {
			statuses = []string{"confirmed"}
		}
	}
	var exclude []string
	for _, ex := range q["status.neq"] {
		ex = strings.ToLower(strings.TrimSpace(ex))
		if ex == "" {
			continue
		}
		if !validBookingStatuses[ex] {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid status.neq")
			return
		}
		exclude = append(exclude, ex)
	}
	limit := 500
	if l := strings.TrimSpace(q.Get("limit")); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > 2000 {
			n = 2000
		}
		limit = n
	}
	var out []bookingResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		sql := `SELECT b.id::text, b.tenant_id, b.facility_id, f.name,
		               b.date::text, b.start_time::text, b.end_time::text,
		               b.customer_name, b.customer_phone, b.status, b.source, b.price, b.override_price, b.payment_status, b.deposit,
		               b.created_at, b.updated_at
		          FROM sports_bookings b
		          JOIN sports_facilities f ON f.id = b.facility_id AND f.tenant_id = b.tenant_id
		         WHERE b.tenant_id = $1`
		args := []any{idn.TenantID()}
		idx := 2
		placeholders := make([]string, 0, len(statuses))
		for _, st := range statuses {
			placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
			args = append(args, st)
			idx++
		}
		sql += fmt.Sprintf(" AND b.status IN (%s)", strings.Join(placeholders, ","))
		for _, ex := range exclude {
			sql += fmt.Sprintf(" AND b.status != $%d", idx)
			args = append(args, ex)
			idx++
		}
		if dateStr != "" {
			sql += fmt.Sprintf(" AND b.date = $%d::date", idx)
			args = append(args, dateStr)
			idx++
		}
		// Date RANGE (from/to), for the payments screen: a booking from
		// three days ago can still be unpaid, and the list only accepted a
		// single exact date, so the UI had no way to reach it. from is
		// inclusive, to is EXCLUSIVE (same convention as the payments and
		// expenses endpoints) so a caller can pass the next day's date.
		// Defaults to DESC ordering: newest first is what a collections
		// view wants, and the ASC default combined with LIMIT 500 silently
		// hid every recent booking once a tenant passed 500 rows.
		rangeStart := strings.TrimSpace(q.Get("from"))
		rangeEnd := strings.TrimSpace(q.Get("to"))
		desc := false
		if rangeStart != "" {
			sql += fmt.Sprintf(" AND b.date >= $%d::date", idx)
			args = append(args, rangeStart)
			idx++
			desc = true
		}
		if rangeEnd != "" {
			sql += fmt.Sprintf(" AND b.date < $%d::date", idx)
			args = append(args, rangeEnd)
			idx++
			desc = true
		}
		if facilityID != "" {
			sql += fmt.Sprintf(" AND b.facility_id = $%d::uuid", idx)
			args = append(args, facilityID)
			idx++
		}
		order := "b.date, b.start_time"
		if desc {
			order = "b.date DESC, b.start_time DESC"
		}
		sql += fmt.Sprintf(" ORDER BY %s LIMIT %d", order, limit)
		rows, err := tx.Query(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b bookingResp
			var createdAt, updatedAt time.Time
			if err := rows.Scan(&b.ID, &b.TenantID, &b.FacilityID, &b.FacilityName,
				&b.Date, &b.StartTime, &b.EndTime, &b.CustomerName, &b.CustomerPhone,
				&b.Status, &b.Source, &b.Price, &b.OverridePrice, &b.PaymentStatus, &b.Deposit,
				&createdAt, &updatedAt); err != nil {
				return err
			}
			b.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			b.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
			out = append(out, b)
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
	var b bookingResp
	var createdAt, updatedAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT b.id::text, b.tenant_id, b.facility_id, f.name,
			        b.date::text, b.start_time::text, b.end_time::text,
			        b.customer_name, b.customer_phone, b.status, b.source, b.price, b.override_price, b.payment_status, b.deposit,
			        b.created_at, b.updated_at
			   FROM sports_bookings b
			   JOIN sports_facilities f ON f.id = b.facility_id AND f.tenant_id = b.tenant_id
			  WHERE b.id = $1::uuid AND b.tenant_id = $2`,
			id, idn.TenantID(),
		).Scan(&b.ID, &b.TenantID, &b.FacilityID, &b.FacilityName,
			&b.Date, &b.StartTime, &b.EndTime, &b.CustomerName, &b.CustomerPhone,
			&b.Status, &b.Source, &b.Price, &b.OverridePrice, &b.PaymentStatus, &b.Deposit,
			&createdAt, &updatedAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	b.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	b.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, b)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Status        *string  `json:"status"`
		CustomerName  *string  `json:"customer_name"`
		CustomerPhone *string  `json:"customer_phone"`
		Price         *float64 `json:"price"`
		OverridePrice *float64 `json:"override_price"`
		Source        *string  `json:"source"`
		Deposit       *float64 `json:"deposit"`
		EndTime       *string  `json:"end_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	// end_time (extend/release booking): HH:MM format checked here,
	// ordering vs start_time checked inside the tx.
	if req.EndTime != nil {
		if _, err := time.Parse("15:04", *req.EndTime); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "end_time must be HH:MM")
			return
		}
	}
	// Owner-only guard for price overrides
	if req.OverridePrice != nil && !idn.IsOwner() {
		records.WriteJSONError(w, http.StatusForbidden, "owner only")
		return
	}
	if req.Price != nil && !idn.IsOwner() {
		records.WriteJSONError(w, http.StatusForbidden, "owner only")
		return
	}
	// Status transition validation (Centrum parity)
	needsStatusCheck := req.Status != nil
	var currentStatus string
	if needsStatusCheck {
		if !validBookingStatuses[*req.Status] {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
	}
	sets := []string{"updated_at = now()"}
	args := []any{}
	idx := 1
	if req.Status != nil {
		sets = append(sets, "status = $"+strconv.Itoa(idx))
		args = append(args, *req.Status)
		idx++
	}
	if req.CustomerName != nil {
		sets = append(sets, "customer_name = $"+strconv.Itoa(idx))
		args = append(args, *req.CustomerName)
		idx++
	}
	if req.CustomerPhone != nil {
		sets = append(sets, "customer_phone = $"+strconv.Itoa(idx))
		args = append(args, *req.CustomerPhone)
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
	if req.OverridePrice != nil {
		if *req.OverridePrice < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "override_price must be >= 0")
			return
		}
		sets = append(sets, "override_price = $"+strconv.Itoa(idx))
		args = append(args, *req.OverridePrice)
		idx++
	}
	if req.Source != nil {
		if *req.Source != "whatsapp" && *req.Source != "walk_in" {
			records.WriteJSONError(w, http.StatusBadRequest, "source must be whatsapp or walk_in")
			return
		}
		sets = append(sets, "source = $"+strconv.Itoa(idx))
		args = append(args, *req.Source)
		idx++
	}
	if req.Deposit != nil {
		if *req.Deposit < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "deposit must be >= 0")
			return
		}
		sets = append(sets, "deposit = $"+strconv.Itoa(idx))
		args = append(args, *req.Deposit)
		idx++
	}
	if req.EndTime != nil {
		sets = append(sets, "end_time = $"+strconv.Itoa(idx)+"::time")
		args = append(args, *req.EndTime)
		idx++
	}
	if len(sets) == 1 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	var affected int64
	var transitionErr error
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// end_time ordering vs the stored start_time (extend/release).
		if req.EndTime != nil {
			var start string
			if err := tx.QueryRow(r.Context(),
				`SELECT start_time::text FROM sports_bookings WHERE id=$1::uuid AND tenant_id=$2`, id, idn.TenantID()).Scan(&start); err != nil {
				return err
			}
			endT, _ := time.Parse("15:04", *req.EndTime)
			startT, serr := time.Parse("15:04:05", start)
			if serr != nil {
				startT, _ = time.Parse("15:04", start)
			}
			if !endT.After(startT) {
				transitionErr = fmt.Errorf("end_time must be after start_time")
				return transitionErr
			}
		}
		// Validate transition against current status
		if needsStatusCheck {
			if err := tx.QueryRow(r.Context(),
				`SELECT status FROM sports_bookings WHERE id=$1::uuid AND tenant_id=$2`, id, idn.TenantID()).Scan(&currentStatus); err != nil {
				return err
			}
			allowed := validTransitions[currentStatus]
			ok := false
			for _, a := range allowed {
				if a == *req.Status {
					ok = true
					break
				}
			}
			if !ok {
				transitionErr = fmt.Errorf("cannot change status from %s to %s", currentStatus, *req.Status)
				return transitionErr
			}
		}
		// Built here, after the current status is known, so the
		// compare-and-set predicate can be appended. See buildUpdateSQL.
		sql, extra := buildUpdateSQL(sets, idx, needsStatusCheck)
		runArgs := args
		if extra > 0 {
			runArgs = append(append([]any{}, args...), currentStatus)
		}
		tag, err := tx.Exec(r.Context(), sql, runArgs...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		if affected == 0 {
			// With a status guard this means another writer moved the row
			// between the SELECT above and here, so the transition this
			// request was validated against never happened. Without one
			// (a price/notes edit) it just means no such row, and the
			// caller gets a 404 from the check after the transaction.
			if needsStatusCheck {
				transitionErr = fmt.Errorf("status already changed by another staff (booking %s is no longer %s)", id, currentStatus)
				return transitionErr
			}
		}
		// bump_no_show: when a booking becomes no_show, increment the
		// linked customer's no_show_count (Centrum parity).
		if affected > 0 && req.Status != nil && *req.Status == "no_show" {
			if _, err := tx.Exec(r.Context(),
				`UPDATE sports_customers SET no_show_count = no_show_count + 1
				  WHERE tenant_id=$1 AND (
				    id = (SELECT customer_id FROM sports_bookings WHERE id=$2::uuid AND customer_id IS NOT NULL)
				    OR phone = (SELECT customer_phone FROM sports_bookings WHERE id=$2::uuid)
				  )`,
				idn.TenantID(), id); err != nil {
				return err
			}
		}
		return nil
	})
	if transitionErr != nil {
		records.WriteJSONError(w, http.StatusConflict, transitionErr.Error())
		return
	}
	if err != nil {
		// Overlap with another active booking → friendly 409, not raw PG text.
		if strings.Contains(err.Error(), "exclusion constraint") {
			records.WriteJSONError(w, http.StatusConflict, "slot already booked")
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

// del cancels a booking.
//
// Two things about this handler are load-bearing.
//
// First, it is a SOFT cancel, and it has to stay one. The row survives
// with status='cancelled' so the payment ledger, the no-show history and
// the audit trail remain readable. A hard DELETE would cascade into
// sports_payments (ON DELETE CASCADE, migration 014) and destroy the
// financial record that the payment_status trigger and every report read.
// Losing that on a revenue dispute is far worse than keeping a row.
//
// Second, the state is now checked, in the same UPDATE that writes it.
// It used to be `WHERE id = ... AND tenant_id = ...` with no status
// condition at all, so a booking that was already completed could be
// dragged back to 'cancelled' — erasing a real court event and freeing
// a slot that was actually played. Only pending and confirmed are
// cancellable; completed and cancelled are terminal, and checked_in /
// no_show describe a slot that was really used. Matching on the status
// inside the UPDATE makes this a compare-and-set, so two staff members
// pressing cancel at once cannot both win against different states.
//
// Any authenticated staff role may cancel: the PATCH transition already
// allows both owner and pegawai to reach 'cancelled' from these states,
// so restricting DELETE would be a UI-only distinction, not a boundary.
func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var currentStatus string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_bookings SET status = 'cancelled', updated_at = now()
			  WHERE id = $1::uuid AND tenant_id = $2 AND status IN (`+cancellableFromSQL+`)`,
			id, idn.TenantID())
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		// Nothing matched. Distinguish "no such booking in this tenant"
		// (404) from "exists but is not cancellable" (409) so the UI can
		// tell the user which happened.
		err = tx.QueryRow(r.Context(),
			`SELECT status FROM sports_bookings WHERE id = $1::uuid AND tenant_id = $2`,
			id, idn.TenantID()).Scan(&currentStatus)
		if err != nil {
			return err // pgx.ErrNoRows -> 404
		}
		return fmt.Errorf("%w: current status is %s", errNotCancellable, currentStatus)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if errors.Is(err, errNotCancellable) {
		records.WriteJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		slog.Error("booking cancel", "err", err)
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "cancelled"})
}
