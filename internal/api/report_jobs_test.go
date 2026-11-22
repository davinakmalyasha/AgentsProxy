package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/dbtest"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// TestReportJobs_BuildAndDownload pins migration 064's deferred export
// path: the worker claims one pending job with the internal token,
// builds it, and the capability link redirects to a real download.
func TestReportJobs_BuildAndDownload(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	rurl := os.Getenv("TEST_REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("TEST_DATABASE_URL / TEST_REDIS_URL not set — skipping integration test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := db.MigrateAll(ctx, admin); err != nil {
		t.Fatal(err)
	}
	runtimePool, err := db.NewPool(ctx, toReportRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	rdb.FlushDB(ctx)

	// Package globals used by the ticket mint/storage.
	agentstools.ReportRDB = rdb
	agentstools.ReportsDir = t.TempDir()
	agentstools.ReportStorage = agentstools.LocalReportStore{}

	const tenant = "rj-test"
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cpool, err := db.NewPool(cctx, adminDSN)
		if err != nil {
			return
		}
		defer cpool.Close()
		cpool.Exec(cctx, `DELETE FROM report_jobs WHERE tenant_id = $1`, tenant)
		cpool.Exec(cctx, `DELETE FROM report_objects WHERE tenant_id = $1`, tenant)
		cpool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})
	schema := dbtest.SharedSchema(t, ctx, admin, "project_centrum", tenant)
	if err := dbtest.ResetSchema(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM clients WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, db_schema)
		 VALUES ('rjt', $1, 'rj-test-hash', 1000000, 'active', $2)`, tenant, schema); err != nil {
		t.Fatal(err)
	}

	jobsH := &ReportJobs{Pool: runtimePool, AdminPool: admin, Token: "test-worker-token"}
	reportsH := &Reports{Pool: runtimePool}
	r := chi.NewRouter()
	r.Post("/v1/internal/report-jobs/build", jobsH.Build)
	r.Get("/v1/reports/jobs/{id}", jobsH.Get)
	r.Get("/v1/reports/{token}", reportsH.Download)
	srv := httptest.NewServer(r)
	defer srv.Close()

	enqueue := func() string {
		t.Helper()
		var id string
		if err := admin.QueryRow(ctx,
			`INSERT INTO report_jobs (tenant_id, from_date, to_date, kind)
			 VALUES ($1, '2026-10-01', '2026-10-02', 'semua') RETURNING id::text`, tenant).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	build := func(token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/internal/report-jobs/build", nil)
		if token != "" {
			req.Header.Set("X-Report-Worker-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Wrong token never claims.
	jobID := enqueue()
	if resp := build("wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong token status=%d want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Worker builds the job.
	resp := build("test-worker-token")
	if resp.StatusCode != 200 {
		t.Fatalf("build status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	var status string
	if err := admin.QueryRow(ctx,
		`SELECT status FROM report_jobs WHERE id = $1::uuid`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("job status=%q want done", status)
	}

	// Capability link redirects to a real download.
	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	jobResp, err := noFollow.Get(srv.URL + "/v1/reports/jobs/" + jobID + ".xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer jobResp.Body.Close()
	if jobResp.StatusCode != 302 {
		t.Fatalf("job link status=%d want 302", jobResp.StatusCode)
	}
	loc := jobResp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/v1/reports/") || !strings.HasSuffix(loc, ".xlsx") {
		t.Fatalf("redirect location=%q", loc)
	}
	fileResp, err := http.Get(srv.URL + loc)
	if err != nil {
		t.Fatal(err)
	}
	defer fileResp.Body.Close()
	if fileResp.StatusCode != 200 {
		t.Fatalf("download status=%d", fileResp.StatusCode)
	}
	if ct := fileResp.Header.Get("Content-Type"); !strings.Contains(ct, "spreadsheetml") {
		t.Errorf("content-type=%q", ct)
	}
	if n := fileResp.ContentLength; n >= 0 && n < 1000 {
		t.Errorf("download %d bytes, want a plausible xlsx", n)
	}

	// Pending jobs answer 409, not a broken download.
	pendingID := enqueue()
	pResp, err := http.Get(srv.URL + "/v1/reports/jobs/" + pendingID + ".xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer pResp.Body.Close()
	if pResp.StatusCode != 409 {
		t.Fatalf("pending job status=%d want 409", pResp.StatusCode)
	}

	// Repeat clicks re-mint the SAME object (066): one manifest row,
	// rotated hash. On the pre-066 code the second mint hit the
	// unique(object_key) constraint and the failure path deleted the
	// shared object — breaking the first link too.
	againResp, err := noFollow.Get(srv.URL + "/v1/reports/jobs/" + jobID + ".xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer againResp.Body.Close()
	if againResp.StatusCode != 302 {
		t.Fatalf("second click status=%d want 302", againResp.StatusCode)
	}
	loc2 := againResp.Header.Get("Location")
	if loc2 == "" || loc2 == loc {
		t.Fatalf("second click location=%q, want a fresh token", loc2)
	}
	file2, err := http.Get(srv.URL + loc2)
	if err != nil {
		t.Fatal(err)
	}
	defer file2.Body.Close()
	if file2.StatusCode != 200 {
		t.Fatalf("second download status=%d want 200", file2.StatusCode)
	}
}

// toReportRuntimeDSN rewrites a DSN to app_runtime (the runtime role).
func toReportRuntimeDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "app_runtime" + rest[at:]
}
