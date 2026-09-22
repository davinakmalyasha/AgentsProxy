// Package availability implements the "availability" feature: busy
// intervals per facility for a date (bookings + court blocks), so the
// landing page can show live free slots by polling. WebSocket
// realtime is a documented future upgrade.
//
// DONE: Phase 7 (realtime-lite) / STANDARDS A, D, F
package availability

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "availability"

func init() { Register() }

func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "", // route-only, read-only
		DefaultOn:     false,
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	r.Get("/", h.get)
}

type handler struct {
	pool *pgxpool.Pool
	// Short in-process cache: the landing board polls this endpoint
	// every ~5s per open tab. A 3s TTL absorbs the poll burst without
	// going to Postgres each time — busy intervals can be up to 3s
	// stale, which is invisible for a "free slot" board and far kinder
	// to the DB. Per-instance is fine: it's a cache, not a source of
	// truth (a write just isn't visible for ≤3s on this instance).
	cache   map[string]cachedAvail
	cacheMu sync.Mutex
}

type cachedAvail struct {
	at      time.Time
	payload map[string]any
}

// cacheTTL bounds how long a built availability payload is reused.
const cacheTTL = 3 * time.Second

type busyInterval struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Kind  string `json:"kind"` // booked | blocked
}

type facilityAvailability struct {
	FacilityID string         `json:"facility_id"`
	Name       string         `json:"name"`
	OpenTime   string         `json:"open_time"`
	CloseTime  string         `json:"close_time"`
	Busy       []busyInterval `json:"busy"`
}

// get returns availability for ?date=YYYY-MM-DD (default today).
// For badminton-only tenants like centrum, ?type=badminton filters to badminton courts only.
// Shared sports table stays — other tenants (soccer, padel) use their own type.
func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	typ := r.URL.Query().Get("type")

	key := idn.TenantID() + "|" + date + "|" + typ
	now := time.Now()
	h.cacheMu.Lock()
	hit, ok := h.cache[key]
	h.cacheMu.Unlock()
	if ok && now.Sub(hit.at) < cacheTTL {
		records.WriteJSON(w, http.StatusOK, hit.payload)
		return
	}

	payload, err := h.build(r.Context(), idn.TenantID(), date, typ)
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}

	h.cacheMu.Lock()
	if h.cache == nil {
		h.cache = map[string]cachedAvail{}
	}
	h.cache[key] = cachedAvail{at: time.Now(), payload: payload}
	h.cacheMu.Unlock()
	records.WriteJSON(w, http.StatusOK, payload)
}

// build runs the 3-query availability read and returns the payload.
func (h *handler) build(ctx context.Context, tenantID, date, typ string) (map[string]any, error) {
	type facInfo struct {
		id, name, open, close string
	}
	var facilities []facInfo
	// facility → busy intervals
	busyByFac := map[string][]busyInterval{}

	err := records.WithTenantTx(ctx, h.pool, tenantID, func(tx pgx.Tx) error {
		q := `SELECT id::text, name, open_time::text, close_time::text FROM sports_facilities
			  WHERE tenant_id=$1 AND status='active'`
		args := []any{tenantID}
		if typ != "" {
			q += ` AND type=$2`
			args = append(args, typ)
		}
		q += ` ORDER BY name`
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f facInfo
			if err := rows.Scan(&f.id, &f.name, &f.open, &f.close); err != nil {
				return err
			}
			facilities = append(facilities, f)
			busyByFac[f.id] = nil
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Booked intervals (non-cancelled/no_show).
		brows, err := tx.Query(ctx,
			`SELECT facility_id::text, start_time::text, end_time::text FROM sports_bookings
			  WHERE tenant_id=$1 AND date=$2::date AND status NOT IN ('cancelled','no_show')
			  ORDER BY start_time`,
			tenantID, date)
		if err != nil {
			return err
		}
		defer brows.Close()
		for brows.Next() {
			var fid, s, e string
			if err := brows.Scan(&fid, &s, &e); err != nil {
				return err
			}
			if _, ok := busyByFac[fid]; ok {
				busyByFac[fid] = append(busyByFac[fid], busyInterval{Start: shortTime(s), End: shortTime(e), Kind: "booked"})
			}
		}
		if err := brows.Err(); err != nil {
			return err
		}

		// Court blocks (facility_id NULL = all courts).
		crows, err := tx.Query(ctx,
			`SELECT COALESCE(facility_id::text,''), start_time::text, end_time::text FROM sports_court_blocks
			  WHERE tenant_id=$1 AND block_date=$2::date
			  ORDER BY start_time`,
			tenantID, date)
		if err != nil {
			return err
		}
		defer crows.Close()
		for crows.Next() {
			var fid, s, e string
			if err := crows.Scan(&fid, &s, &e); err != nil {
				return err
			}
			bi := busyInterval{Start: shortTime(s), End: shortTime(e), Kind: "blocked"}
			if fid == "" {
				// All-courts block → every facility.
				for f := range busyByFac {
					busyByFac[f] = append(busyByFac[f], bi)
				}
			} else if _, ok := busyByFac[fid]; ok {
				busyByFac[fid] = append(busyByFac[fid], bi)
			}
		}
		return crows.Err()
	})
	if err != nil {
		return nil, err
	}

	out := make([]facilityAvailability, 0, len(facilities))
	for _, f := range facilities {
		out = append(out, facilityAvailability{
			FacilityID: f.id,
			Name:       f.name,
			OpenTime:   shortTime(f.open),
			CloseTime:  shortTime(f.close),
			Busy:       busyByFac[f.id],
		})
	}
	return map[string]any{"date": date, "facilities": out}, nil
}

func shortTime(s string) string {
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}
