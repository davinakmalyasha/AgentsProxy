// Chat-generated report downloads (WS2).
//
// GET /v1/reports/{token}[.xlsx] streams a chat-built .xlsx. The token
// IS the auth: 64-hex, tenant-scoped, 30-min TTL in Redis. No session
// needed, so Telegram users and logged-in staff can both download.
// Quota keys are never touched here.
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
)

// Reports serves token-authed report files.
type Reports struct {
	RDB *redis.Client
}

// Download streams the .xlsx for a valid token (Range-capable, no
// full-file buffering — reports can be megabytes).
func (h *Reports) Download(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(chi.URLParam(r, "token"), ".xlsx")
	f, filename, err := agentstools.OpenReportFile(token)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "link kadaluarsa atau tidak dikenal — minta lagi ke bot")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "link kadaluarsa atau tidak dikenal — minta lagi ke bot")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// StartSweeper runs the disk reclaim in the background every 10
// minutes. Keeping it out of the request path matters: downloads are
// bursty (LLM links clicked by many users) and an O(tenants × files)
// walk on every click is wasted synchronous work. Tickets in Redis
// expire on their own (30 min); this only reclaims disk.
func (h *Reports) StartSweeper(stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	sweepOldReports()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sweepOldReports()
		}
	}
}

// reportsRoot is where chat-generated .xlsx files live. A var so tests
// can point it at a temp dir instead of the real working tree.
var reportsRoot = filepath.Join("data", "reports")

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
