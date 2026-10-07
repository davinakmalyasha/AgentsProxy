// DONE: PRD 6.4 / STANDARDS H (ping DB+Redis, 500 if down)
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

type Healthz struct {
	DB    *pgxpool.Pool
	RDB   *redis.Client
	Build string
	// HealthyTenants lists slugs serving traffic. Empty (not nil) means
	// "report only quarantined ones". Kept as data, not a callback, so
	// /healthz never blocks on config.
	HealthyTenants []string
}

type healthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      bool   `json:"db"`
	Redis   bool   `json:"redis"`
	// Tenants maps quarantined slug → reason. Absent slugs are healthy
	// (or unknown). No prompt bodies, no contacts — status only.
	Tenants map[string]string `json:"tenants,omitempty"`
}

func (h *Healthz) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	resp := healthzResponse{Status: "ok", Version: h.Build}
	dbOK := false
	if err := h.DB.Ping(ctx); err == nil {
		dbOK = true
	}
	resp.DB = dbOK
	rdsOK := false
	if err := h.RDB.Ping(ctx).Err(); err == nil {
		rdsOK = true
	}
	resp.Redis = rdsOK
	if !dbOK || !rdsOK {
		resp.Status = "degraded"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	for slug, e := range tenanthealth.Snapshot() {
		if resp.Tenants == nil {
			resp.Tenants = map[string]string{}
		}
		resp.Tenants[slug] = e.Reason
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
