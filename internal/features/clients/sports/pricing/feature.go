// Package pricing implements the "pricing" feature: date-aware
// pricing rules (day/evening rates, open/close hours). Port of
// CentrumBadminton's pricing_rules. The bookings handler uses
// ResolvePrice to compute a booking's price from the active rule.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

const featureName = "pricing"

func init() { Register() }

func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "", // custom table
		DefaultOn:     false,
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Post("/apply_hours", h.applyHours)
	r.Get("/{id}", records.WithValidID(h.getOne))
	r.Patch("/{id}", records.WithValidID(h.patch))
	r.Delete("/{id}", records.WithValidID(h.del))
}

type handler struct {
	pool *pgxpool.Pool
}

type ruleResp struct {
	ID           string  `json:"id"`
	TenantID     string  `json:"tenant_id"`
	ValidFrom    string  `json:"valid_from"`
	OpenTime     string  `json:"open_time"`
	CloseTime    string  `json:"close_time"`
	DayStart     string  `json:"day_start"`
	DayEnd       string  `json:"day_end"`
	PriceDay     float64 `json:"price_day"`
	PriceEvening float64 `json:"price_evening"`
	IsActive     bool    `json:"is_active"`
	CreatedAt    string  `json:"created_at"`
}

// Rule is a pricing rule row (exported for the bookings handler).
type Rule struct {
	ValidFrom    time.Time
	OpenTime     string
	CloseTime    string
	DayStart     string
	DayEnd       string
	PriceDay     float64
	PriceEvening float64
}

// ResolvePrice returns the active rule for a date + the price for a
// time range. Day = start >= day_start AND end <= day_end, else evening.
// Returns (rule, price, err). ErrNoRule when no rule exists yet.
var ErrNoRule = fmt.Errorf("no pricing rule for date")

func (h *handler) resolveRule(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time) (*Rule, error) {
	var r Rule
	var validFrom time.Time
	err := tx.QueryRow(ctx,
		`SELECT valid_from, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening
		   FROM sports_pricing_rules
		  WHERE tenant_id=$1 AND is_active AND valid_from <= $2::date
		  ORDER BY valid_from DESC LIMIT 1`,
		tenantID, date).Scan(&validFrom, &r.OpenTime, &r.CloseTime, &r.DayStart, &r.DayEnd, &r.PriceDay, &r.PriceEvening)
	if err != nil {
		// Distinguish "this tenant has no rule covering that date" from a
		// genuine database failure. resolveRule used to hand the raw
		// pgx.ErrNoRows back, so a caller that wanted to say "set up a
		// pricing rule first" instead saw an error it could not classify
		// — which is how the bookings handler ended up swallowing it and
		// committing the booking at Rp0.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoRule
		}
		return nil, err
	}
	r.ValidFrom = validFrom
	return &r, nil
}

// RuleFor returns the active pricing rule for a date, or ErrNoRule.
// Exported for the bookings handler, which checks the rule's opening
// window before accepting a slot.
//
// This window was declared authoritative when the table was created
// ("the rule active for a booking's date determines day/evening prices
// AND open/close hours") but nothing ever read it: PriceFor only uses
// day_start/day_end to pick a rate, so the enforced hours came from
// sports_facilities instead — a different table with a different default
// (06:00-22:00 vs 07:00-23:00).
func RuleFor(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time) (*Rule, error) {
	h := &handler{}
	return h.resolveRule(ctx, tx, tenantID, date)
}

// PriceFor computes the price for a (date, start, end) booking using
// the active pricing rule for the tenant. Exported for the bookings
// handler. Uses the given tx so it's atomic with the booking insert.
func PriceFor(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time, start, end time.Time) (float64, error) {
	h := &handler{pool: nil}
	return h.PriceFor(ctx, tx, tenantID, date, start, end)
}

// PriceFor computes the price for a (date, start, end) booking.
func (h *handler) PriceFor(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time, start, end time.Time) (float64, error) {
	r, err := h.resolveRule(ctx, tx, tenantID, date)
	if err != nil {
		return 0, err
	}
	// Duration in hours (fractional).
	hours := end.Sub(start).Hours()
	dayStart, _ := time.Parse("15:04", normTime(r.DayStart))
	dayEnd, _ := time.Parse("15:04", normTime(r.DayEnd))
	pricePerHour := r.PriceEvening
	if !start.Before(dayStart) && !end.After(dayEnd) {
		pricePerHour = r.PriceDay
	}
	return pricePerHour * hours, nil
}

func normTime(s string) string {
	if len(s) > 5 && strings.HasSuffix(s, ":00") {
		return s[:5]
	}
	return s
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	var req struct {
		ValidFrom    string  `json:"valid_from"`
		OpenTime     string  `json:"open_time"`
		CloseTime    string  `json:"close_time"`
		DayStart     string  `json:"day_start"`
		DayEnd       string  `json:"day_end"`
		PriceDay     float64 `json:"price_day"`
		PriceEvening float64 `json:"price_evening"`
		IsActive     *bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.ValidFrom == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "valid_from required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", req.ValidFrom); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "valid_from must be YYYY-MM-DD")
		return
	}
	if req.PriceDay < 0 || req.PriceEvening < 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "prices must be >= 0")
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	open, closeT := defTime(req.OpenTime, "07:00"), defTime(req.CloseTime, "23:00")
	dayStart, dayEnd := defTime(req.DayStart, "07:00"), defTime(req.DayEnd, "17:00")
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_pricing_rules (tenant_id, valid_from, open_time, close_time, day_start, day_end, price_day, price_evening, is_active)
			 VALUES ($1,$2::date,$3::time,$4::time,$5::time,$6::time,$7,$8,$9) RETURNING id::text`,
			idn.TenantID(), req.ValidFrom, open, closeT, dayStart, dayEnd, req.PriceDay, req.PriceEvening, isActive,
		).Scan(&id)
	})
	if err != nil {
		if isUniqueViolation(err) {
			records.WriteJSONError(w, http.StatusConflict, "a rule already exists for that valid_from")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func defTime(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var out []ruleResp
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, tenant_id, valid_from::text, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening, is_active, created_at
			   FROM sports_pricing_rules WHERE tenant_id=$1 ORDER BY valid_from DESC LIMIT $2`,
			idn.TenantID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ruleResp
			var createdAt time.Time
			if err := rows.Scan(&r.ID, &r.TenantID, &r.ValidFrom, &r.OpenTime, &r.CloseTime, &r.DayStart, &r.DayEnd,
				&r.PriceDay, &r.PriceEvening, &r.IsActive, &createdAt); err != nil {
				return err
			}
			r.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, r)
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
	var rr ruleResp
	var createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, valid_from::text, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening, is_active, created_at
			   FROM sports_pricing_rules WHERE id=$1::uuid AND tenant_id=$2`,
			id, idn.TenantID(),
		).Scan(&rr.ID, &rr.TenantID, &rr.ValidFrom, &rr.OpenTime, &rr.CloseTime, &rr.DayStart, &rr.DayEnd,
			&rr.PriceDay, &rr.PriceEvening, &rr.IsActive, &createdAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	rr.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, rr)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		OpenTime     *string  `json:"open_time"`
		CloseTime    *string  `json:"close_time"`
		DayStart     *string  `json:"day_start"`
		DayEnd       *string  `json:"day_end"`
		PriceDay     *float64 `json:"price_day"`
		PriceEvening *float64 `json:"price_evening"`
		IsActive     *bool    `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{}
	args := []any{}
	idx := 1
	type kv struct {
		col string
		val any
	}
	var vals []kv
	if req.OpenTime != nil {
		vals = append(vals, kv{"open_time", *req.OpenTime + "::time"})
	}
	if req.CloseTime != nil {
		vals = append(vals, kv{"close_time", *req.CloseTime + "::time"})
	}
	if req.DayStart != nil {
		vals = append(vals, kv{"day_start", *req.DayStart + "::time"})
	}
	if req.DayEnd != nil {
		vals = append(vals, kv{"day_end", *req.DayEnd + "::time"})
	}
	if req.PriceDay != nil {
		if *req.PriceDay < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "price_day must be >= 0")
			return
		}
		vals = append(vals, kv{"price_day", strconv.FormatFloat(*req.PriceDay, 'f', 2, 64)})
	}
	if req.PriceEvening != nil {
		if *req.PriceEvening < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "price_evening must be >= 0")
			return
		}
		vals = append(vals, kv{"price_evening", strconv.FormatFloat(*req.PriceEvening, 'f', 2, 64)})
	}
	if req.IsActive != nil {
		vals = append(vals, kv{"is_active", strconv.FormatBool(*req.IsActive)})
	}
	if len(vals) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	for _, v := range vals {
		sets = append(sets, v.col+" = $"+strconv.Itoa(idx))
		args = append(args, v.val)
		idx++
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_pricing_rules SET %s WHERE id=$%d::uuid AND tenant_id=$%d",
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

// applyHours is the single owner-facing control for venue opening hours.
//
// Opening hours used to live in two tables with two different defaults:
// sports_facilities (06:00-22:00) and sports_pricing_rules (07:00-23:00).
// The booking form offers slots from the RULE, but the backend enforced
// the FACILITY, so a 22:30 slot could be priced and then refused — and
// there was no single place to fix the mismatch, because changing one
// table just moved the disagreement to the other.
//
// This writes both in one transaction, so there is one setting to change
// and the two cannot end up disagreeing. A facility can still be closed
// early on purpose afterwards via the facilities endpoint; that is a
// deliberate per-court override rather than a stale default.
func (h *handler) applyHours(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	var req struct {
		OpenTime  string `json:"open_time"`
		CloseTime string `json:"close_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	open, err := time.Parse("15:04", strings.TrimSpace(req.OpenTime))
	if err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "open_time must be HH:MM")
		return
	}
	closeT, err := time.Parse("15:04", strings.TrimSpace(req.CloseTime))
	if err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "close_time must be HH:MM")
		return
	}
	if !closeT.After(open) {
		records.WriteJSONError(w, http.StatusBadRequest, "close_time must be after open_time")
		return
	}

	var rules, courts int
	err = records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Every rule, not just the newest: a rule dated in the future
		// would otherwise keep opening at the old time and start
		// disagreeing again the day it takes effect.
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_pricing_rules SET open_time = $1::time, close_time = $2::time
			  WHERE tenant_id = $3`,
			open.Format("15:04"), closeT.Format("15:04"), idn.TenantID())
		if err != nil {
			return err
		}
		rules = int(tag.RowsAffected())
		tag2, err := tx.Exec(r.Context(),
			`UPDATE sports_facilities SET open_time = $1::time, close_time = $2::time, updated_at = now()
			  WHERE tenant_id = $3`,
			open.Format("15:04"), closeT.Format("15:04"), idn.TenantID())
		if err != nil {
			return err
		}
		courts = int(tag2.RowsAffected())
		return nil
	})
	if err != nil {
		slog.Error("apply venue hours", "err", err)
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{
		"open_time":      open.Format("15:04"),
		"close_time":     closeT.Format("15:04"),
		"rules_updated":  rules,
		"facilities_set": courts,
	})
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
			`DELETE FROM sports_pricing_rules WHERE id=$1::uuid AND tenant_id=$2`,
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
