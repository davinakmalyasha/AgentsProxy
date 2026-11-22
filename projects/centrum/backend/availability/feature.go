// Package availability implements the "availability" feature: busy
// intervals per facility for a date (bookings + court blocks), so the
// landing page can show live free slots by polling. WebSocket
// realtime is a documented future upgrade.
//
// DONE: Phase 7 (realtime-lite) / STANDARDS A, D, F
package availability

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "availability"

func init() { Register() }

func Register() {
	features.RegisterProject("centrum", "sports", features.Feature{
		Name:          featureName,
		Kind:          "", // route-only, read-only
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
		PublicRead:    true, // landing page polls this without a token
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
	// truth (a write just isn't visible for ~3s on this instance).
	//
	// Bounded: maxAvailCacheEntries caps tenants × dates × types, and
	// every write evicts expired entries first, so the map cannot grow
	// without bound on a public polling endpoint.
	cache   map[string]cachedAvail
	cacheMu sync.Mutex
}

// maxAvailCacheEntries bounds the availability cache: one entry per
// (tenant, date, type). A venue polls a handful of dates; 512 is far
// above any real keyspace and keeps the cap a non-issue in practice.
const maxAvailCacheEntries = 512

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
		// WIB, not the server's local time. The proxy's own prompt says
		// "today" means today in Jakarta, and every SQL filter in this
		// codebase already compares in Asia/Jakarta — so a UTC host would
		// default to yesterday for seven hours a day and the landing board
		// would quietly show the wrong day's availability.
		date = agentstools.WibNow().Format("2006-01-02")
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
	// Evict expired entries first so the map cannot grow without bound;
	// then enforce the cap by dropping one entry (every entry is at
	// most seconds old, so any drop is cheap and self-healing).
	now2 := time.Now()
	for k, v := range h.cache {
		if now2.Sub(v.at) >= cacheTTL {
			delete(h.cache, k)
		}
	}
	if len(h.cache) >= maxAvailCacheEntries {
		for k := range h.cache {
			delete(h.cache, k)
			break
		}
	}
	h.cache[key] = cachedAvail{at: time.Now(), payload: payload}
	h.cacheMu.Unlock()
	records.WriteJSON(w, http.StatusOK, payload)
}

// build runs the availability read and returns the payload.
//
// One statement, one round trip: facilities LEFT JOINed to their busy
// intervals (bookings + blocks aggregated as JSONB in SQL). The old
// shape ran 3 sequential queries in a transaction (facilities, then
// bookings, then blocks) plus the tx framing — 6 round trips per cache
// miss on the highest-QPS endpoint. An all-courts block
// (facility_id NULL) fans out in the UNION, so the Go fan-out loop is
// gone too.
func (h *handler) build(ctx context.Context, tenantID, date, typ string) (map[string]any, error) {
	out := make([]facilityAvailability, 0, 16)
	err := records.WithTenantTx(ctx, h.pool, tenantID, func(tx pgx.Tx) error {
		q := `SELECT f.id::text, f.name, f.open_time::text, f.close_time::text,
		              COALESCE((
		                SELECT jsonb_agg(x ORDER BY x->>'start', x->>'end')
		                  FROM (
		                    SELECT jsonb_build_object('start', to_char(b.start_time, 'HH24:MI'),
		                                              'end', to_char(b.end_time, 'HH24:MI'),
		                                              'kind', 'booked') AS x
		                      FROM sports_bookings b
		                     WHERE b.tenant_id = $1 AND b.date = $2::date
		                       AND b.status NOT IN ('cancelled', 'no_show')
		                       AND b.facility_id = f.id
		                    UNION ALL
		                    SELECT jsonb_build_object('start', to_char(k.start_time, 'HH24:MI'),
		                                              'end', to_char(k.end_time, 'HH24:MI'),
		                                              'kind', 'blocked')
		                      FROM sports_court_blocks k
		                     WHERE k.tenant_id = $1 AND k.block_date = $2::date
		                       AND (k.facility_id = f.id OR k.facility_id IS NULL)
		                  ) sub
		              ), '[]'::jsonb)
		         FROM sports_facilities f
		        WHERE f.tenant_id = $1 AND f.deleted_at IS NULL AND f.status = 'active'`
		args := []any{tenantID, date}
		if typ != "" {
			q += ` AND f.type = $3`
			args = append(args, typ)
		}
		// Courts are ones, not thousands — but no query ships unbounded.
		// 500 is far above any venue; hitting it means a different product.
		q += ` ORDER BY f.name LIMIT 500`
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var fa facilityAvailability
			var open, close string
			var busyRaw []byte
			if err := rows.Scan(&fa.FacilityID, &fa.Name, &open, &close, &busyRaw); err != nil {
				return err
			}
			fa.OpenTime = shortTime(open)
			fa.CloseTime = shortTime(close)
			// Preserve the old shape exactly: no busy intervals
			// serializes as null, not [].
			if string(busyRaw) != "[]" {
				var busy []busyInterval
				if err := json.Unmarshal(busyRaw, &busy); err != nil {
					return err
				}
				for i := range busy {
					busy[i].Start = shortTime(busy[i].Start)
					busy[i].End = shortTime(busy[i].End)
				}
				fa.Busy = busy
			}
			out = append(out, fa)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"date": date, "facilities": out}, nil
}

func shortTime(s string) string {
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}
