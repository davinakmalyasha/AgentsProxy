package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminSLOReport exercises the route end to end: admin auth, handler,
// report shape, and the honesty rules (every class present; low volume says
// insufficient rather than inventing a percentile).
func TestAdminSLOReport(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/admin/slo?window=15", nil)
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin slo status=%d body=%s, want 200", resp.StatusCode, raw)
	}
	var rep struct {
		WindowMinutes int `json:"window_minutes"`
		MinSamples    int `json:"min_samples"`
		Classes       []struct {
			Class    string  `json:"class"`
			Requests uint64  `json:"requests"`
			Status   string  `json:"status"`
			P95Ms    float64 `json:"p95_ms"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if rep.WindowMinutes != 15 || rep.MinSamples <= 0 {
		t.Errorf("meta: window=%d min_samples=%d", rep.WindowMinutes, rep.MinSamples)
	}
	if len(rep.Classes) == 0 {
		t.Fatal("no classes reported")
	}
	seen := map[string]bool{}
	for _, c := range rep.Classes {
		seen[c.Class] = true
		if c.Status != "ok" && c.Status != "insufficient" && c.Status != "breach" {
			t.Errorf("class %q has unknown status %q", c.Class, c.Status)
		}
		if c.Requests == 0 && c.Status != "insufficient" {
			t.Errorf("class %q with 0 requests must be insufficient, got %q", c.Class, c.Status)
		}
	}
	for _, want := range []string{"public", "staff", "admin", "agent", "chat", "other"} {
		if !seen[want] {
			t.Errorf("class %q missing from report", want)
		}
	}

	// Unauthenticated callers must not read operational aggregates.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/admin/slo", nil)
	resp2 := mustReq(t, req2)
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized && resp2.StatusCode != http.StatusForbidden {
		t.Errorf("unauthenticated slo status=%d, want 401/403", resp2.StatusCode)
	}
}
