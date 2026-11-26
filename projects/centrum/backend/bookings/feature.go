// Package bookings implements the sports-vertical "bookings"
// feature. Uses a custom table (sports_bookings) with an exclusion
// constraint that makes double-booking IMPOSSIBLE at the database
// level — no application code can accidentally overlap a facility's
// time range, no matter how concurrent the requests.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package bookings

import (
	"context"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/pgerr"
	"github.com/davinakmalyasha/agents-proxy/internal/projecthooks"
	"github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/pricing"
)

const featureName = "bookings"

// SchemaVersion is this binary's bookings contract (see tenant_schema_contracts
// + records.CheckContractTx). Bump when the shape this code reads changes,
// and keep the previous shape readable for one release so rollback is a row.
const SchemaVersion = 1

func init() { Register() }

// Register registers the bookings feature. Safe to call from tests
// after features.ResetForTest().
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

type bookingReq struct {
	FacilityID    string `json:"facility_id"`
	Date          string `json:"date"` // YYYY-MM-DD
	StartTime     string `json:"start_time"`
	EndTime       string `json:"end_time"`
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	// Price is accepted for wire compatibility but NOT used: the price is
	// always computed from the active pricing rule. Honouring a
	// client-supplied figure let a stale or hand-typed number commit, and
	// the "non-owners cannot be trusted" check below only covered the
	// other half of the branch. Use OverridePrice for a deliberate
	// adjustment.
	Price float64 `json:"price"`
	// OverridePrice was silently dropped on create: this struct had no
	// such field and the INSERT did not name the column, so the booking
	// form's adjusted price was accepted and then discarded, leaving the
	// computed price in place. Owner-only, same as PATCH.
	OverridePrice *float64 `json:"override_price"`
	Deposit       float64  `json:"deposit"`
	Status        string   `json:"status"`
	Source        string   `json:"source"` // whatsapp | walk_in
	Notes         string   `json:"notes"`
	// RecurringSeriesID links a generated slot to the subscription that
	// produced it. Accepted and stored; without it a subscription's weekly
	// bookings were indistinguishable from ordinary ones.
	RecurringSeriesID string `json:"recurring_series_id"`
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
	// RecurringSeriesID links a generated slot back to the subscription that
	// produced it. The frontend has always sent this and the backend had
	// nowhere to put it, so subscription-generated bookings were
	// indistinguishable from ordinary ones and could not be counted,
	// extended, or cancelled with their series.
	RecurringSeriesID string  `json:"recurring_series_id,omitempty"`
	PaymentStatus     string  `json:"payment_status"`
	Deposit           float64 `json:"deposit"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// nullableUUID turns the empty string into SQL NULL. The column is a
// uuid: passing "" would be a 22P02 invalid-input error rather than the
// "this booking isn't part of a subscription" the caller meant.
func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// errOutsideRuleHours is returned when a slot falls outside the active
// pricing rule's opening window. Separate from errOutsideHours because the
// remedy is different: the venue hours are a setting, the rule window is
// the same setting, and the message should name which one refused.
var errOutsideRuleHours = errors.New("booking outside the pricing rule's opening hours")

// errSlotBlocked is returned when the requested slot overlaps a court block
// (maintenance, private event). Mapped to 409 so the UI can offer the owner
// a way through rather than showing a generic failure.
var errSlotBlocked = errors.New("slot is blocked")

// errDuplicateKey is the in-tx signal for "idempotency pre-check found the
// original row". It never leaves create: the caller maps it to the
// deduplicated reply below.
var errDuplicateKey = errors.New("idempotent replay")

// blockReason returns the reason the slot is blocked, or "" when it is free.
//
// The database trigger (migration 037) enforces the same rule, because the
// agent writes straight to sports_bookings and would otherwise bypass this
// check entirely. This exists so the caller gets a sentence instead of a
// raw Postgres exclusion error -- and so the block feature can be extended
// (an override flag, say) in one place.
//
// Overlap is half-open: a block ending at 12:00 does not refuse a booking
// starting at 12:00.
func blockReason(ctx context.Context, tx pgx.Tx, tenantID, facilityID string, date time.Time, start, end time.Time) (string, error) {
	var reason string
	err := tx.QueryRow(ctx,
		`SELECT reason FROM sports_court_blocks
		  WHERE tenant_id=$1 AND block_date=$2::date
		    AND (facility_id=$3::uuid OR facility_id IS NULL)
		    AND start_time < $4::time AND end_time > $5::time
		  LIMIT 1`,
		tenantID, date, facilityID, end, start).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return reason, nil
}

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
	// Idempotency-Key (optional phase, same as payments/kantin/topups): a
	// repeated key returns the original booking. Absent behaves as before.
	idemKey, haveIdem, idemErr := records.IdempotencyKey(w, r)
	if idemErr != nil {
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
	// override_price is the owner-only channel for a deliberately adjusted
	// price, on create exactly as on PATCH.
	if req.OverridePrice != nil {
		if !idn.IsOwner() {
			records.WriteJSONError(w, http.StatusForbidden, "owner only")
			return
		}
		if *req.OverridePrice < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "override_price must be >= 0")
			return
		}
	}

	var (
		id       string
		facility string
		openStr  string
		closeStr string
	)
	err = records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Contract gate: a tenant rolled forward past this binary fails
		// closed here (428 below), scoped to that tenant alone.
		if err := records.CheckContractTx(r.Context(), tx, idn.TenantID(), featureName, SchemaVersion); err != nil {
			return err
		}
		// Idempotent replay inside the same transaction (single-tx, so the
		// deadlock retry re-reads instead of double-firing).
		if haveIdem {
			var existing, existingStatus string
			qerr := tx.QueryRow(r.Context(),
				`SELECT id::text, status FROM sports_bookings WHERE tenant_id=$1 AND idempotency_key=$2`,
				idn.TenantID(), idemKey).Scan(&existing, &existingStatus)
			if qerr == nil {
				id, status = existing, existingStatus
				return errDuplicateKey
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return qerr
			}
		}
		// Validate the facility exists + is active + within open hours.
		err := tx.QueryRow(r.Context(),
			`SELECT name, open_time::text, close_time::text FROM sports_facilities
			  WHERE id = $1::uuid AND tenant_id = $2 AND status = 'active' AND deleted_at IS NULL`,
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
		// The price is always the server's number. The old code branched on
		// role and let an owner's client-supplied `price` through, and —
		// worse — swallowed every pricing error and fell back to 0:
		//
		//   price = 0 // fallback, akan diisi trigger? keep 0 for now
		//
		// Nothing ever filled it in. With no pricing rule covering the
		// date, a booking committed at Rp0: it looked free on the
		// schedule, counted as a booking in reports, and stayed unpaid
		// forever because payment_status treats a 0 price as unpaid.
		// Refusing the booking is the honest outcome — the owner has to
		// set up pricing before taking money for a slot.
		rule, price, err := pricing.RuleAndPriceFor(r.Context(), tx, idn.TenantID(), date, start, end)
		if err != nil {
			return err
		}
		// Also enforce the active rule's opening window. The facility
		// check above only covered sports_facilities, whose default
		// (06:00-22:00) is an hour narrower at each end than the pricing
		// rule's (07:00-23:00) — and the booking form offers slots from
		// the RULE, so a 22:30 slot could be priced and then refused.
		// Both are now checked and both come from the same owner setting
		// (see applyHours), so they only diverge when a court is
		// deliberately closed early.
		ruleOpen, e1 := time.Parse("15:04", strings.TrimSuffix(rule.OpenTime, ":00"))
		ruleClose, e2 := time.Parse("15:04", strings.TrimSuffix(rule.CloseTime, ":00"))
		if e1 != nil || e2 != nil {
			return fmt.Errorf("rule hours unparseable: %s-%s", rule.OpenTime, rule.CloseTime)
		}
		if start.Before(ruleOpen) || end.After(ruleClose) {
			return fmt.Errorf("%w: %s-%s", errOutsideRuleHours, rule.OpenTime, rule.CloseTime)
		}
		// A blocked court is not bookable. Checked before the INSERT so the
		// caller gets the reason; the trigger is the backstop for paths
		// that write here directly.
		if reason, err := blockReason(r.Context(), tx, idn.TenantID(), req.FacilityID, date, start, end); err != nil {
			return err
		} else if reason != "" {
			return fmt.Errorf("%w: %s", errSlotBlocked, reason)
		}
		// Link a customer by phone (Centrum's upsert pattern) when the
		// phone matches an existing directory entry.
		var customerID *string
		if req.CustomerPhone != "" {
			var cid string
			if err := tx.QueryRow(r.Context(),
				`SELECT id::text FROM sports_customers WHERE tenant_id=$1 AND phone=$2 AND deleted_at IS NULL`,
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
		// override_price is stored alongside the computed price, never
		// instead of it. Everything downstream reads
		// COALESCE(override_price, price), so keeping both preserves the
		// rule-based figure for comparison and lets the owner see what
		// the adjustment was.
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_bookings
			   (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status, source, price, override_price, deposit, notes, customer_id, created_by, recurring_series_id, idempotency_key)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16::uuid,NULLIF($17,'')) RETURNING id::text`,
			idn.TenantID(), req.FacilityID, date, start, end, req.CustomerName, req.CustomerPhone, status, source, price,
			req.OverridePrice, req.Deposit, req.Notes, customerID, createdByArg, nullableUUID(req.RecurringSeriesID), idemKey,
		).Scan(&id)
	})
	if err != nil {
		// Idempotent replay: the pre-check found the original inside the
		// same transaction (id/status already pointed at it).
		if errors.Is(err, errDuplicateKey) {
			records.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "status": status, "deduplicated": true})
			return
		}
		// Lost the insert race with a duplicate key: return the winner.
		if haveIdem && records.IsUniqueViolation(err) {
			var existing, existingStatus string
			qerr := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
				return tx.QueryRow(r.Context(),
					`SELECT id::text, status FROM sports_bookings WHERE tenant_id=$1 AND idempotency_key=$2`,
					idn.TenantID(), idemKey).Scan(&existing, &existingStatus)
			})
			if qerr == nil {
				records.WriteJSON(w, http.StatusOK, map[string]any{"id": existing, "status": existingStatus, "deduplicated": true})
				return
			}
		}
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
		if errors.Is(err, errOutsideRuleHours) {
			records.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A block is a conflict, not a bad request: the slot is valid in
		// itself, the court is simply unavailable.
		if errors.Is(err, errSlotBlocked) {
			records.WriteJSONError(w, http.StatusConflict, err.Error())
			return
		}
		// No pricing rule for that date. A 400 with an actionable message,
		// not a 500: this is a setup step the owner has to take, and the
		// old behaviour (commit at Rp0) was worse than refusing.
		if errors.Is(err, pricing.ErrNoRule) {
			records.WriteJSONError(w, http.StatusBadRequest,
				"belum ada aturan harga untuk tanggal ini — set harga di Pengaturan dulu")
			return
		}
		// Rollout gate: this tenant's contract is ahead of this binary.
		// 428 (distinct from 500/503) so it reads as "deploy first",
		// not as a crash or a fence.
		if errors.Is(err, records.ErrContractMismatch) {
			records.WriteJSONError(w, http.StatusPreconditionRequired, "schema contract mismatch")
			return
		}
		slog.Error("booking create", "err", err)
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if projecthooks.BookingNotify != nil && status == "confirmed" {
		projecthooks.BookingNotify(r.Context(), idn.TenantID(), req.CustomerPhone, fmt.Sprintf(
			"Booking confirmed: %s at %s, %s %s-%s",
			req.CustomerName, facility, date.Format("Mon Jan 2"), req.StartTime, req.EndTime))
	}
	records.WriteJSON(w, http.StatusCreated, map[string]string{"id": id, "status": status})
}

// list returns bookings for a date and/or facility (the daily
// schedule view). Status filter: ?status=a,b (default confirmed for
// backward compat), plus repeatable ?status.neq= to exclude. ?limit
// bounds the rows (default 500, max 2000). ?id= narrows to one booking.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	q := r.URL.Query()
	dateStr := q.Get("date")
	facilityID := q.Get("facility_id")
	if facilityID != "" {
		if _, err := uuid.Parse(strings.TrimSpace(facilityID)); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "facility_id must be a uuid")
			return
		}
		facilityID = strings.TrimSpace(facilityID)
	}
	// ?id=<uuid> narrows to a single booking. The frontend connector has
	// always sent this for `.eq("id", …)` — the shim's id mapping is
	// table-agnostic — but this handler never read it. The parameter was
	// silently dropped, so an id lookup returned the first page of the
	// list (oldest-first, 500 rows) and the caller got whichever booking
	// happened to sort first. It reads as "not found" once the target
	// falls outside that page, which is how a single-row lookup turns
	// into an intermittent "Booking tidak ditemukan".
	idFilter := strings.TrimSpace(q.Get("id"))
	if idFilter != "" {
		if _, err := uuid.Parse(idFilter); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "id must be a uuid")
			return
		}
	}
	// An id lookup implies a specific row, so the status filter must not
	// hide it: a completed or cancelled booking is exactly the one a
	// caller re-reading a row after an update wants back.
	statuses := []string{"confirmed"}
	if idFilter != "" {
		statuses = nil
	}
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
	out := make([]bookingResp, 0, limit+1)
	truncated := false
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		sql := `SELECT b.id::text, b.tenant_id, b.facility_id, f.name,
		               b.date::text, b.start_time::text, b.end_time::text,
		               b.customer_name, b.customer_phone, b.status, b.source, b.price, b.override_price, b.payment_status, b.deposit,
		               COALESCE(b.recurring_series_id::text, ''),
		               b.created_at, b.updated_at
		          FROM sports_bookings b
		          JOIN sports_facilities f ON f.id = b.facility_id AND f.tenant_id = b.tenant_id
		         WHERE b.tenant_id = $1`
		args := []any{idn.TenantID()}
		idx := 2
		if idFilter != "" {
			sql += fmt.Sprintf(" AND b.id = $%d::uuid", idx)
			args = append(args, idFilter)
			idx++
		}
		if len(statuses) > 0 {
			placeholders := make([]string, 0, len(statuses))
			for _, st := range statuses {
				placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
				args = append(args, st)
				idx++
			}
			sql += fmt.Sprintf(" AND b.status IN (%s)", strings.Join(placeholders, ","))
		}
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
		rangeStart := strings.TrimSpace(q.Get("from"))
		rangeEnd := strings.TrimSpace(q.Get("to"))
		hasRange := false
		if rangeStart != "" {
			sql += fmt.Sprintf(" AND b.date >= $%d::date", idx)
			args = append(args, rangeStart)
			idx++
			hasRange = true
		}
		if rangeEnd != "" {
			sql += fmt.Sprintf(" AND b.date < $%d::date", idx)
			args = append(args, rangeEnd)
			idx++
			hasRange = true
		}
		if facilityID != "" {
			sql += fmt.Sprintf(" AND b.facility_id = $%d::uuid", idx)
			args = append(args, facilityID)
			idx++
		}
		// Order newest-first unless the caller is reading one day, which is
		// the schedule grid and wants chronological order.
		//
		// ASC was the only order, and paired with the 500-row cap it made
		// the cap hit the OLDEST bookings: a tenant with more than 500 rows
		// asking for its newest bookings — the payments screen, the
		// dashboard — got a page of history instead and no error. A single
		// ?date= is the one case where ascending is the correct answer, so
		// that is the only case that keeps it.
		desc := !(dateStr != "" && !hasRange)
		order := "b.date DESC, b.start_time DESC"
		if !desc {
			order = "b.date, b.start_time"
		}
		// One row past the cap is how truncation is detected without a
		// second COUNT query. `truncated` is reported rather than implied,
		// because a silently shortened list is indistinguishable from a
		// complete one — a dashboard would show real-but-incomplete
		// numbers with no way to tell.
		sql += fmt.Sprintf(" ORDER BY %s LIMIT %d", order, limit+1)
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
				&b.RecurringSeriesID,
				&createdAt, &updatedAt); err != nil {
				return err
			}
			b.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			b.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
			out = append(out, b)
			if len(out) > limit {
				// The probe row. Drop it and stop.
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
	// `items` is always a list, never null: the connector and several
	// pages iterate it unconditionally.
	if out == nil {
		out = []bookingResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "truncated": truncated})
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
			        COALESCE(b.recurring_series_id::text, ''),
			        b.created_at, b.updated_at
			   FROM sports_bookings b
			   JOIN sports_facilities f ON f.id = b.facility_id AND f.tenant_id = b.tenant_id
			  WHERE b.id = $1::uuid AND b.tenant_id = $2`,
			id, idn.TenantID(),
		).Scan(&b.ID, &b.TenantID, &b.FacilityID, &b.FacilityName,
			&b.Date, &b.StartTime, &b.EndTime, &b.CustomerName, &b.CustomerPhone,
			&b.Status, &b.Source, &b.Price, &b.OverridePrice, &b.PaymentStatus, &b.Deposit,
			&b.RecurringSeriesID,
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
	// repriceArg is the 1-based position of the price argument when an
	// extension is being applied. The slot is reserved here, before the
	// id/tenant arguments are appended, because the SET list and argument
	// order are baked into the statement by the time the transaction
	// opens; the real value is filled in inside the transaction once the
	// stored start_time is known. Reserving rather than appending is what
	// keeps the new price from colliding with the id and tenant_id
	// parameters. The reservation is unconditional, because the stored value
	// is not known until the transaction opens; `reprice` is cleared inside
	// the tx when the new end_time is not actually an extension.
	repriceArg := 0
	reprice := true
	if req.EndTime != nil {
		sets = append(sets, "end_time = $"+strconv.Itoa(idx)+"::time")
		args = append(args, *req.EndTime)
		idx++
		sets = append(sets, "price = $"+strconv.Itoa(idx))
		args = append(args, nil) // placeholder, replaced inside the tx
		repriceArg = idx
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
		// Single read of the row for every path that needs it (extend /
		// release validation, release price, status transition). Was 3
		// sequential SELECTs of the same row (start/date/end, then price,
		// then status) plus a second rule lookup on extension.
		var start, dateStr, storedEnd, currentStatus string
		var currentPrice float64
		if req.EndTime != nil || needsStatusCheck {
			if err := tx.QueryRow(r.Context(),
				`SELECT start_time::text, date::text, end_time::text, price, status FROM sports_bookings
			  WHERE id=$1::uuid AND tenant_id=$2`,
				id, idn.TenantID()).Scan(&start, &dateStr, &storedEnd, &currentPrice, &currentStatus); err != nil {
				return err
			}
		}
		if req.EndTime != nil {
			endT, _ := time.Parse("15:04", *req.EndTime)
			startT, serr := time.Parse("15:04:05", start)
			if serr != nil {
				startT, _ = time.Parse("15:04", start)
			}
			storedEndT, eerr := time.Parse("15:04:05", storedEnd)
			if eerr != nil {
				storedEndT, _ = time.Parse("15:04", storedEnd)
			}
			if !endT.After(startT) {
				transitionErr = fmt.Errorf("end_time must be after start_time")
				return transitionErr
			}
			// A SHORTENING is a release, not an extension, and must not
			// reprice.
			//
			// "Kosongkan" sets end_time to the current time to free the rest
			// of the slot, and its own tooltip promises "uang tetap
			// dihitung" — the hours already played are still owed. Repricing
			// shortened the booking to the rate for the time actually used,
			// so releasing a 3-hour booking after 1 hour cut the committed
			// value by two thirds, and the schedule and the agent's "teresan"
			// figure then disagreed with what was charged.
			//
			// This slot is reserved whenever end_time is present (the caller
			// cannot know the stored value), but only filled in when the slot
			// got LONGER. See repriceArg below.
			if !endT.After(storedEndT) {
				reprice = false
			}
			if reprice {
				// Extending used to cost the customer nothing: the row kept the
				// price computed for the ORIGINAL window, so extra court time
				// was given away and never appeared in revenue. Recompute for
				// the new window instead.
				//
				// This runs for any role on purpose. The figure comes from the
				// tenant's own pricing rule, never from the request, so it is
				// not a trust question — and refusing staff would only push
				// people back to editing the price by hand.
				date, derr := time.Parse("2006-01-02", dateStr)
				if derr != nil {
					return derr
				}
				newPrice, perr := pricing.PriceFor(r.Context(), tx, idn.TenantID(), date, startT, endT)
				if perr != nil {
					return perr
				}
				// Fill the slot reserved before the transaction opened. The
				// SET list and arg order are fixed by then — id and tenant
				// already occupy the positions after it.
				args[repriceArg-1] = newPrice
			} else {
				// A release keeps the committed value, so the reserved price
				// argument must be the CURRENT price rather than the
				// placeholder nil — otherwise this would write NULL and erase
				// it. Already fetched above; no second SELECT.
				args[repriceArg-1] = currentPrice
			}
		}
		// Validate transition against current status (already fetched).
		if needsStatusCheck {
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
				  WHERE tenant_id=$1 AND deleted_at IS NULL AND (
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
		// Overlap with another active booking — friendly 409, not raw PG text.
		// Classified by SQLSTATE: the block trigger raises the same 23P01,
		// and message-text matching breaks on any server wording change.
		// A named constraint is the overlap guard; an unnamed 23P01 is
		// the block trigger.
		if pgerr.IsExclusion(err) {
			if pgerr.Constraint(err) != "" {
				records.WriteJSONError(w, http.StatusConflict, "slot already booked")
			} else {
				records.WriteJSONError(w, http.StatusConflict, "slot is blocked")
			}
			return
		}
		// An extension cannot be repriced without a rule. Say so instead
		// of a 500 — the old code never reached here because the price was
		// simply left stale.
		if errors.Is(err, pricing.ErrNoRule) {
			records.WriteJSONError(w, http.StatusBadRequest,
				"belum ada aturan harga untuk tanggal ini — set harga di Pengaturan dulu")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			records.WriteJSONError(w, http.StatusNotFound, "not found")
			return
		}
		slog.Error("booking update", "err", err)
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
	// Optional cancellation note (union rule with the cancel_booking tool:
	// when money moved, how offline cash was handled). Absent behaves
	// exactly as before; over-long is 400, never silently cut.
	note := strings.TrimSpace(r.URL.Query().Get("note"))
	if len(note) > 500 {
		records.WriteJSONError(w, http.StatusBadRequest, "note too long (max 500 chars)")
		return
	}
	var currentStatus string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_bookings SET status = 'cancelled', updated_at = now(),
			        notes = CASE WHEN $3 = '' THEN notes WHEN notes = '' THEN $3 ELSE notes || E'\n' || $3 END
			  WHERE id = $1::uuid AND tenant_id = $2 AND status IN (`+cancellableFromSQL+`)`,
			id, idn.TenantID(), note)
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
