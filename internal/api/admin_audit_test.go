package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAdminAuditWritesRow proves an admin mutation leaves a durable,
// attributable row with the final status. The row is inserted before the
// handler, so this also fails if the audit middleware is removed from the
// route.
func TestAdminAuditWritesRow(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()

	origin := fmt.Sprintf("https://audit-%d.example.com", time.Now().UnixNano())
	body := fmt.Sprintf(`{"tenant_id":"alpha","origin":%q,"label":"audit-test"}`, origin)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = h.pool.Exec(ctx, `DELETE FROM frontend_origins WHERE origin = $1`, origin)
		_, _ = h.pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE target LIKE '%' || $1 || '%'`, origin)
	})

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/admin/origins", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create status=%d body=%s, want 201", resp.StatusCode, bodyBytes)
	}

	var actor, action, target, requestID string
	var status int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT actor, action, target, status, request_id
		   FROM public.admin_audit_log
		  WHERE target LIKE '%' || $1 || '%'
		  ORDER BY id DESC LIMIT 1`, origin).
		Scan(&actor, &action, &target, &status, &requestID); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	if !strings.HasPrefix(actor, "admin-key:") {
		t.Errorf("actor=%q, want an ADMIN_API_KEY fingerprint", actor)
	}
	if action != "POST /v1/admin/origins" {
		t.Errorf("action=%q, want POST /v1/admin/origins", action)
	}
	if !strings.Contains(target, "tenant_id=alpha") || !strings.Contains(target, origin) {
		t.Errorf("target=%q, want tenant_id and origin", target)
	}
	if status != http.StatusCreated {
		t.Errorf("audited status=%d, want 201", status)
	}
	if requestID == "" {
		t.Error("audit row has no request_id")
	}
}
