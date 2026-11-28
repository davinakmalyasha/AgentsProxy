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
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/pgerr"
	"golang.org/x/sync/singleflight"
)

const featureName = "pricing"

// SchemaVersion is this binary's pricing contract (see tenant_schema_contracts
// + records.CheckContractTx). Bump when the shape this code reads changes,
// and keep the previous shape readable for one release so rollback is a row.
const SchemaVersion = 1

func init() { Register() }

func Register() {
	features.RegisterProject("centrum", "sports", features.Feature{
		Name:          featureName,
		Kind:          "", // custom table
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
		PublicRead:    true, // GET is a landing-page read; writes stay gated
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
	// Small in-process list cache: the rule list changes rarely but
	// is read on every landing-page load. 30s TTL mirrors facilities
	// (edge already holds 60s). Writes evict the tenant's entries.
	// Separate from ruleCache below, which caches the single active
	// rule for booking price resolution.
	listCache   map[string]cachedList
	listCacheMu sync.Mutex
	sf          singleflight.Group
}

// maxListCacheEntries bounds the pricing list cache.
const maxListCacheEntries = 256

type cachedList struct {
	at    time.Time
	items []ruleResp
}

// listCacheTTL bounds how long a built rule list is reused.
const listCacheTTL = 30 * time.Second

// evictList drops one tenant's cached lists after a rule write.
func (h *handler) evictList(tenantID string) {
	prefix := tenantID + "|"
	h.listCacheMu.Lock()
	for k := range h.listCache {
		if strings.HasPrefix(k, prefix) {
			delete(h.listCache, k)
		}
	}
	h.listCacheMu.Unlock()
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

// Small in-process rule cache: rules change rarely, but every booking
// create, extension reprice, and get_pricing call reads the active rule.
// The contract gate still runs per call (safety); only the rule SELECT is
// cached, for 30s per (tenant, date). Writes clear the cache so a new rule
// is visible immediately (at most one stale read per instance per write,
// bounded by the TTL anyway).
var (
	ruleCacheMu sync.RWMutex
	ruleCache   = map[string]cachedRule{}
)

type cachedRule struct {
	at   time.Time
	rule Rule
}

const ruleCacheTTL = 30 * time.Second

func ruleCacheKey(tenantID string, date time.Time) string {
	return tenantID + "|" + date.Format("2006-01-02")
}

func ruleCacheGet(tenantID string, date time.Time) (Rule, bool) {
	k := ruleCacheKey(tenantID, date)
	now := time.Now()
	ruleCacheMu.RLock()
	e, ok := ruleCache[k]
	ruleCacheMu.RUnlock()
	if !ok || now.Sub(e.at) >= ruleCacheTTL {
		return Rule{}, false
	}
	return e.rule, true
}

func ruleCacheSet(tenantID string, date time.Time, r Rule) {
	k := ruleCacheKey(tenantID, date)
	ruleCacheMu.Lock()
	if len(ruleCache) >= 1024 {
		// Bounded: rules are tiny, tenants are few; drop one arbitrary
		// entry rather than scanning for expired ones on the hot path.
		for kk := range ruleCache {
			delete(ruleCache, kk)
			break
		}
	}
	ruleCache[k] = cachedRule{at: time.Now(), rule: r}
	ruleCacheMu.Unlock()
}

func ruleCacheClear() {
	ruleCacheMu.Lock()
	ruleCache = map[string]cachedRule{}
	ruleCacheMu.Unlock()
}

func (h *handler) resolveRule(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time) (*Rule, error) {
	// Contract gate: a tenant rolled forward past this binary refuses
	// here, before any price is computed — one choke point for RuleFor,
	// PriceFor, and every caller (bookings, tools, availability).
	if err := records.CheckContractTx(ctx, tx, tenantID, featureName, SchemaVersion); err != nil {
		return nil, err
	}
	// Cache the rule SELECT only; the contract check above still runs per
	// call so a rolled-forward tenant never gets a stale allow.
	day := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
	if cached, ok := ruleCacheGet(tenantID, day); ok {
		r := cached
		return &r, nil
	}
	var r Rule
	var validFrom time.Time
	err := tx.QueryRow(ctx,
		`SELECT valid_from, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening
		   FROM sports_pricing_rules
		  WHERE tenant_id=$1 AND is_active AND deleted_at IS NULL AND valid_from <= $2::date
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
	ruleCacheSet(tenantID, day, r)
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
//
// The window is priced HOUR BY HOUR at the day/evening rate, which is the
// rule the whole rest of the product uses: computePrice in the frontend
// (availability.ts) sums one rate per hour, and the same figure is what
// the customer is quoted on the booking form, the landing board and the
// WhatsApp message.
//
// This used to apply ONE rate to the whole window -- the day rate only if
// the entire booking fit inside day_start..day_end, otherwise the evening
// rate for every hour. Any booking straddling the boundary was quoted one
// number and charged another: 16:00-18:00 with day_end 17:00 quoted
// Rp90.000 (one day hour + one evening hour) and committed Rp100.000 (two
// evening hours). That is the same class of bug as the Rp0 pricing
// fallback, one layer up: the customer agreed to a price the server never
// used.
func (h *handler) PriceFor(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time, start, end time.Time) (float64, error) {
	r, err := h.resolveRule(ctx, tx, tenantID, date)
	if err != nil {
		return 0, err
	}
	return priceFromRule(r, start, end), nil
}

// RuleAndPriceFor resolves the active rule once and returns both the
// rule and the computed price. Booking creates need both (the price
// and the opening-hours window); separate PriceFor + RuleFor calls
// would run the contract gate and the rule lookup twice per booking.
func RuleAndPriceFor(ctx context.Context, tx pgx.Tx, tenantID string, date time.Time, start, end time.Time) (*Rule, float64, error) {
	h := &handler{pool: nil}
	r, err := h.resolveRule(ctx, tx, tenantID, date)
	if err != nil {
		return nil, 0, err
	}
	return r, priceFromRule(r, start, end), nil
}

// priceFromRule prices a (start, end) window hour by hour at the
// day/evening rate, matching the frontend's computePrice.
//
// MONEY INVARIANT (whole-rupiah IDR): every amount in the system is a
// whole rupiah — no sen — so float64 is exact for storage and sums
// (all values < 2^53). The one exception is right here: rate ×
// fractional hours (e.g. a 20-minute tail) is inexact binary, so the
// result is rounded to cents before it leaves. Postgres numeric(12,2)
// would round on write anyway; rounding here keeps the Go value, the
// stored value, and the displayed value identical instead of
// 13333.333333333334 vs 13333.33.
func priceFromRule(r *Rule, start, end time.Time) float64 {
	dayStart, _ := time.Parse("15:04", normTime(r.DayStart))
	dayEnd, _ := time.Parse("15:04", normTime(r.DayEnd))
	total := 0.0
	// Walk the window one hour at a time, charging the rate in force at
	// the START of each hour. The final partial hour is charged as a full
	// one, matching the frontend, which iterates `for h = startH; h < endH`.
	// Overlapping court slots are whole hours everywhere in this product,
	// so a fractional end is not a case worth diverging on.
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		rate := r.PriceEvening
		if !t.Before(dayStart) && t.Before(dayEnd) {
			rate = r.PriceDay
		}
		segEnd := t.Add(time.Hour)
		if segEnd.After(end) {
			segEnd = end
		}
		total += rate * segEnd.Sub(t).Hours()
	}
	return math.Round(total*100) / 100
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
		if pgerr.IsUniqueViolation(err) {
			records.WriteJSONError(w, http.StatusConflict, "a rule already exists for that valid_from")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	ruleCacheClear()
	h.evictList(idn.TenantID())
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
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	key := idn.TenantID() + "|" + strconv.Itoa(limit)
	_, isBust := r.URL.Query()["__bench_cb"]
	now := time.Now()
	if !isBust {
		h.listCacheMu.Lock()
		hit, ok := h.listCache[key]
		h.listCacheMu.Unlock()
		if ok && now.Sub(hit.at) < listCacheTTL {
			if idn.IsPublic() {
				records.MarkPublicCache(w, 120, 300)
			}
			records.WriteJSON(w, http.StatusOK, map[string]any{"items": hit.items})
			return
		}
	}
	var out []ruleResp
	_, err, _ := h.sf.Do(key, func() (any, error) {
		built := make([]ruleResp, 0, limit+1)
		rerr := records.WithTenantRead(r.Context(), h.pool, idn.TenantID(),
			`SELECT id::text, tenant_id, valid_from::text, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening, is_active, created_at
			   FROM sports_pricing_rules WHERE tenant_id=$1 AND deleted_at IS NULL ORDER BY valid_from DESC LIMIT $2`,
			[]any{idn.TenantID(), limit},
			func(rows pgx.Rows) error {
				for rows.Next() {
					var r ruleResp
					var createdAt time.Time
					if err := rows.Scan(&r.ID, &r.TenantID, &r.ValidFrom, &r.OpenTime, &r.CloseTime, &r.DayStart, &r.DayEnd,
						&r.PriceDay, &r.PriceEvening, &r.IsActive, &createdAt); err != nil {
						return err
					}
					r.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
					built = append(built, r)
				}
				return rows.Err()
			})
		if rerr != nil {
			return nil, rerr
		}
		out = built
		return built, nil
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	h.listCacheMu.Lock()
	if h.listCache == nil {
		h.listCache = map[string]cachedList{}
	}
	if len(h.listCache) >= maxListCacheEntries {
		now2 := time.Now()
		for k, v := range h.listCache {
			if now2.Sub(v.at) >= listCacheTTL {
				delete(h.listCache, k)
			}
		}
		if len(h.listCache) >= maxListCacheEntries {
			for k := range h.listCache {
				delete(h.listCache, k)
				break
			}
		}
	}
	h.listCache[key] = cachedList{at: time.Now(), items: out}
	h.listCacheMu.Unlock()
	if idn.IsPublic() {
		records.MarkPublicCache(w, 120, 300)
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
	err := records.WithTenantRead(r.Context(), h.pool, idn.TenantID(),
		`SELECT id::text, tenant_id, valid_from::text, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening, is_active, created_at
		   FROM sports_pricing_rules WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
		[]any{id, idn.TenantID()},
		func(rows pgx.Rows) error {
			if !rows.Next() {
				if err := rows.Err(); err != nil {
					return err
				}
				return pgx.ErrNoRows
			}
			return rows.Scan(&rr.ID, &rr.TenantID, &rr.ValidFrom, &rr.OpenTime, &rr.CloseTime, &rr.DayStart, &rr.DayEnd,
				&rr.PriceDay, &rr.PriceEvening, &rr.IsActive, &createdAt)
		})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	rr.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	if idn.IsPublic() {
		records.MarkPublicCache(w, 120, 300)
	}
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
	for _, tf := range []struct {
		val *string
		col string
	}{
		{req.OpenTime, "open_time"},
		{req.CloseTime, "close_time"},
		{req.DayStart, "day_start"},
		{req.DayEnd, "day_end"},
	} {
		if tf.val != nil {
			// Fail closed on format: the raw string is concatenated with
			// ::time below, so garbage would 500 in SQL instead of 400 here.
			if _, err := time.Parse("15:04", normTime(*tf.val)); err != nil {
				records.WriteJSONError(w, http.StatusBadRequest, tf.col+" must be HH:MM")
				return
			}
			vals = append(vals, kv{tf.col, *tf.val + "::time"})
		}
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
	sql := fmt.Sprintf("UPDATE sports_pricing_rules SET %s WHERE id=$%d::uuid AND tenant_id=$%d AND deleted_at IS NULL",
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
	ruleCacheClear()
	h.evictList(idn.TenantID())
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
			  WHERE tenant_id = $3 AND deleted_at IS NULL`,
			open.Format("15:04"), closeT.Format("15:04"), idn.TenantID())
		if err != nil {
			return err
		}
		rules = int(tag.RowsAffected())
		tag2, err := tx.Exec(r.Context(),
			`UPDATE sports_facilities SET open_time = $1::time, close_time = $2::time, updated_at = now()
			  WHERE tenant_id = $3 AND deleted_at IS NULL`,
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
	ruleCacheClear()
	h.evictList(idn.TenantID())
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
		// Tombstone, not DELETE: the rule row is preserved with who/when,
		// hidden from lists/reads/price resolution.
		deletedBy := idn.UserID
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_pricing_rules SET deleted_at = now(), deleted_by = $3
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
	ruleCacheClear()
	h.evictList(idn.TenantID())
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}
