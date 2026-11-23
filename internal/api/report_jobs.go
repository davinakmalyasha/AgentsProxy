package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// ReportJobs runs deferred report builds (migration 064) and serves the
// capability link.
//
// The agent tool enqueues large exports and returns
// /v1/reports/jobs/{id}.xlsx; a pg_cron tick posts to Build with the
// internal worker token; the link answers 409 while pending and 302s to
// a fresh /v1/reports/{token} download once the file exists.
type ReportJobs struct {
	// Pool is the runtime pool: FetchReportData runs under the tenant
	// GUC and MintReportToken uses the 053 definer.
	Pool *pgxpool.Pool
	// AdminPool claims jobs across tenants (like the reminder scheduler
	// on the admin pool).
	AdminPool *pgxpool.Pool
	// Token authenticates the pg_cron worker. Empty disables the
	// endpoint (503) — a missing secret must never leave it open.
	Token string
}

type reportJobClaim struct {
	id       string
	tenant   string
	from, to string
	kind     string
	attempts int
}

// Build claims ONE pending job and builds it. Called by pg_cron via
// pg_net with the X-Report-Worker-Token header.
func (h *ReportJobs) Build(w http.ResponseWriter, r *http.Request) {
	if h.Token == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "report worker disabled")
		return
	}
	got := strings.TrimSpace(r.Header.Get("X-Report-Worker-Token"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.Token)) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "invalid worker token")
		return
	}

	var c reportJobClaim
	err := h.AdminPool.QueryRow(r.Context(),
		`WITH due AS (
		     SELECT id FROM report_jobs
		      WHERE status = 'pending'
		      ORDER BY created_at
		      LIMIT 1
		      FOR UPDATE SKIP LOCKED
		 ), claimed AS (
		     UPDATE report_jobs j
		        SET status = 'working', claimed_at = now(),
		            attempts = attempts + 1, updated_at = now()
		       FROM due
		      WHERE j.id = due.id
		     RETURNING j.id::text, j.tenant_id, j.from_date::text, j.to_date::text,
		               j.kind, j.attempts
		 )
		 SELECT * FROM claimed`).
		Scan(&c.id, &c.tenant, &c.from, &c.to, &c.kind, &c.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]int{"claimed": 0})
		return
	}
	if err != nil {
		slog.Error("report job claim", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "claim failed")
		return
	}

	objectKey, filename, size, buildErr := h.build(r.Context(), c)
	if buildErr != nil {
		h.fail(r.Context(), c, buildErr)
		slog.Warn("report job build failed", "job", middleware.Fingerprint(c.id), "attempt", c.attempts, "err", buildErr)
		writeJSON(w, http.StatusOK, map[string]any{"claimed": 1, "built": 0})
		return
	}
	if _, err := h.AdminPool.Exec(r.Context(),
		`UPDATE report_jobs
		    SET status = 'done', claimed_at = NULL, object_key = $2,
		        filename = $3, size_bytes = $4, error = '', updated_at = now()
		  WHERE id = $1::uuid`,
		c.id, objectKey, filename, size); err != nil {
		slog.Error("report job complete", "job", middleware.Fingerprint(c.id), "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": 1, "built": 1})
}

// build runs the export pipeline with the worker's own budget: it runs
// outside the agent turn, so it may take longer than TurnTimeout.
func (h *ReportJobs) build(ctx context.Context, c reportJobClaim) (objectKey, filename string, size int, err error) {
	bctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	data, err := agentstools.FetchReportData(bctx, h.Pool, c.tenant, c.from, c.to)
	if err != nil {
		return "", "", 0, err
	}
	f, err := agentstools.BuildReportWorkbook(data, c.tenant, c.from, c.to)
	if err != nil {
		return "", "", 0, err
	}
	raw, err := agentstools.WriteWorkbook(f)
	if err != nil {
		return "", "", 0, err
	}
	name, err := agentstools.ReportFilename(c.from, c.to, c.kind)
	if err != nil {
		return "", "", 0, err
	}
	key, err := agentstools.SaveReport(bctx, c.tenant, name, raw)
	if err != nil {
		return "", "", 0, err
	}
	return key, name, len(raw), nil
}

// fail ladders a failed build: three attempts, then dead (same shape as
// reminders).
func (h *ReportJobs) fail(ctx context.Context, c reportJobClaim, buildErr error) {
	status := "pending"
	if c.attempts >= 3 {
		status = "dead"
	}
	if _, err := h.AdminPool.Exec(ctx,
		`UPDATE report_jobs
		    SET status = $2, claimed_at = NULL, error = $3, updated_at = now()
		  WHERE id = $1::uuid`,
		c.id, status, buildErr.Error()); err != nil {
		slog.Error("report job fail", "job", middleware.Fingerprint(c.id), "err", err)
	}
}

// Get serves the capability link: 409 while pending/working, 302 to a
// fresh download ticket when done, 410 when the build died. The random
// UUID is the secret, matching the /v1/reports/{token} model; no
// session is involved.
func (h *ReportJobs) Get(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(chi.URLParam(r, "id"), ".xlsx")
	if _, err := uuid.Parse(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var tenant, status, objectKey, filename, errMsg string
	var size int
	err := h.Pool.QueryRow(r.Context(),
		`SELECT tenant_id, status, object_key, filename, size_bytes, error
		   FROM private.report_job_get($1::uuid)`, id).
		Scan(&tenant, &status, &objectKey, &filename, &size, &errMsg)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "laporan tidak dikenal")
		return
	}
	if err != nil {
		slog.Error("report job lookup", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}

	switch status {
	case "pending", "working":
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusConflict, map[string]string{"status": status})
	case "done":
		token, err := agentstools.MintReportToken(r.Context(), h.Pool, tenant, objectKey, size)
		if err != nil {
			slog.Error("report job mint", "job", middleware.Fingerprint(id), "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		http.Redirect(w, r, "/v1/reports/"+token+".xlsx", http.StatusFound)
	default: // error, dead
		writeJSONError(w, http.StatusGone, "laporan gagal dibuat — minta ulang ke asisten")
	}
}
