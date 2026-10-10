// Chat-generated report downloads (WS2).
//
// GET /v1/reports/{token}[.xlsx] streams a chat-built .xlsx. The token
// IS the auth: 64-hex, tenant-scoped, 30-min TTL in Redis. No session
// needed, so logged-in app users can download from the presented link.
// Quota keys are never touched here.
package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

// Reports serves token-authed report files.
type Reports struct {
	Pool *pgxpool.Pool
}

// Download streams the .xlsx for a valid token (Range-capable).
func (h *Reports) Download(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(chi.URLParam(r, "token"), ".xlsx")
	// One ticket lookup serves both the quarantine fence and the load.
	tenant, filename, data, err := agentstools.ResolveTicket(r.Context(), token)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "link kadaluarsa atau tidak dikenal — minta lagi ke aplikasi")
		return
	}
	if q, reason := tenanthealth.Check(tenant); q {
		slog.Warn("quarantined tenant blocked from reports", "tenant", tenant, "reason", reason)
		writeJSONError(w, http.StatusServiceUnavailable, tenanthealth.ResponseBody)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filename, time.Now(), bytes.NewReader(data))
}

// StartSweeper runs reclaim in the background every 10 minutes:
// manifest-driven purge of expired objects on every backend, plus the
// legacy local-disk walk for pre-manifest files. Keeping it out of the
// request path matters: downloads are bursty (LLM links clicked by many
// users) and an O(tenants × files) walk on every click is wasted
// synchronous work. Tickets in Redis expire on their own (30 min); this
// only reclaims objects.
func (h *Reports) StartSweeper(stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	sweepOnce(h.Pool)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sweepOnce(h.Pool)
		}
	}
}

func sweepOnce(pool *pgxpool.Pool) {
	if pool != nil {
		if _, err := agentstools.PurgeExpiredReports(context.Background(), pool); err != nil {
			// Best-effort: never crash the sweeper on a transient DB error,
			// but expired objects accumulating is a storage leak that must page.
			slog.Error("report purge failed", "err", err)
		}
	}
	// The local-disk walk only makes sense when the active backend IS
	// local disk. On Supabase (or any remote backend) data/reports does
	// not exist and the walk is a guaranteed-empty ReadDir per sweep.
	if ls, ok := agentstools.ReportStorage.(interface{ IsLocal() bool }); ok && ls.IsLocal() {
		sweepOldReports()
	}
}

// reportsRoot is where chat-generated .xlsx files live. A var so tests
// can point it at a temp dir instead of the real working tree; defaults
// to the same directory the tools write (agentstools.ReportsDir).
var reportsRoot = agentstools.ReportsDir

// sweepOldReports deletes report files older than 1h (best-effort).
func sweepOldReports() {
	entries, err := os.ReadDir(reportsRoot)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, tenant := range entries {
		if !tenant.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(reportsRoot, tenant.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".xlsx") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(reportsRoot, tenant.Name(), f.Name()))
			}
		}
	}
}
