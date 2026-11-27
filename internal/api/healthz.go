// DONE: PRD 6.4 / STANDARDS H (ping DB+Redis, 500 if down)
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

type Healthz struct {
	DB    *pgxpool.Pool
	RDB   *redis.Client
	Build string
}

type healthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      bool   `json:"db"`
	Redis   bool   `json:"redis"`
	// TenantsFenced is an aggregate only. Public health must not expose
	// tenant slugs or internal config reasons; the admin tenant-health
	// endpoint carries the detailed view.
	TenantsFenced int `json:"tenants_fenced,omitempty"`
	// Pool visibility for connection-budget tuning: acquired/idle/total
	// come straight from pgxpool.Stat, no extra query. Clients ignore
	// unknown fields; ops uses this to see MAX=2 saturation.
	PoolAcquired uint32 `json:"pool_acquired,omitempty"`
	PoolIdle     uint32 `json:"pool_idle,omitempty"`
	PoolTotal    uint32 `json:"pool_total,omitempty"`
	PoolMax      int32  `json:"pool_max,omitempty"`
}

func (h *Healthz) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	resp := healthzResponse{Status: "ok", Version: h.Build}
	var dbOK, rdsOK bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dbOK = h.DB.Ping(ctx) == nil
	}()
	go func() {
		defer wg.Done()
		rdsOK = h.RDB.Ping(ctx).Err() == nil
	}()
	wg.Wait()
	resp.DB = dbOK
	resp.Redis = rdsOK
	if st := h.DB.Stat(); st != nil {
		resp.PoolAcquired = uint32(st.AcquiredConns())
		resp.PoolIdle = uint32(st.IdleConns())
		resp.PoolTotal = uint32(st.TotalConns())
		resp.PoolMax = st.MaxConns()
	}
	if !dbOK || !rdsOK {
		resp.Status = "degraded"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	resp.TenantsFenced = len(tenanthealth.Snapshot())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
