// Report job enqueueing for deferred (async) exports.
//
// Small ranges still build synchronously inside the agent turn; ranges
// above ReportSyncMaxDays are enqueued instead, because the builder's
// 120s statement_timeout outlives the ~80s turn budget. The worker
// (internal/api report_jobs.go) claims one job per pg_cron tick; the
// job UUID is the download capability.
package agentstools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

// ReportSyncMaxDays is the longest range still built inside the turn.
// A month of rows builds in seconds; anything longer defers.
const ReportSyncMaxDays = 31

// ShouldDeferReport reports whether a range is too long for an in-turn
// build. Extracted so the threshold is unit-tested, not buried in the
// tool.
func ShouldDeferReport(from, to time.Time) bool {
	return to.Sub(from).Hours()/24 > ReportSyncMaxDays
}

// ReportJob is the enqueue result.
type ReportJob struct {
	ID string
}

// EnqueueReportJob persists a deferred export request for the tenant.
// Insert goes through WithTenantTx so the RLS GUC is set exactly like
// every other tenant write.
func EnqueueReportJob(ctx context.Context, pool *pgxpool.Pool, tenantID, from, to, kind string) (ReportJob, error) {
	var job ReportJob
	err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO report_jobs (tenant_id, from_date, to_date, kind)
			 VALUES ($1, $2::date, $3::date, $4) RETURNING id::text`,
			tenantID, from, to, kind).Scan(&job.ID)
	})
	if err != nil {
		return ReportJob{}, err
	}
	return job, nil
}
