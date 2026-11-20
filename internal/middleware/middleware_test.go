package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := mustGet(t, srv.URL+"/x")
	defer resp.Body.Close()
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s=%q want %q", k, got, want)
		}
	}

	// OPTIONS short-circuit must still get the headers and not error.
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/x", nil)
	oresp := mustDo(t, req)
	defer oresp.Body.Close()
	if oresp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("OPTIONS missing security header")
	}
}

func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRequestID(t *testing.T) {
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(CtxRequestID).(string)
		if rid == "" {
			t.Error("no request id in ctx")
		}
		w.WriteHeader(200)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp := mustGet(t, srv.URL)
	defer resp.Body.Close()
	if resp.Header.Get(HeaderRequestID) == "" {
		t.Error("response missing X-Request-ID")
	}
}

func TestRecovery(t *testing.T) {
	h := Recovery()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp := mustGet(t, srv.URL)
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}
}

func TestCORS_Wildcard(t *testing.T) {
	h := CORS([]string{"*"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("OPTIONS", srv.URL, nil)
	req.Header.Set("Origin", "https://x.test")
	resp := mustDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("OPTIONS status=%d want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("allow-origin=%q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestCORS_AllowList(t *testing.T) {
	h := CORS([]string{"https://a.test", "https://b.test"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Origin", "https://a.test")
	resp := mustDo(t, req)
	defer resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://a.test" {
		t.Errorf("allow-origin=%q", resp.Header.Get("Access-Control-Allow-Origin"))
	}

	req2, _ := http.NewRequest("GET", srv.URL, nil)
	req2.Header.Set("Origin", "https://evil.test")
	resp2 := mustDo(t, req2)
	defer resp2.Body.Close()
	if resp2.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disallowed origin got allow-origin=%q", resp2.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestRateLimit_Real(t *testing.T) {
	rurl := os.Getenv("TEST_REDIS_URL")
	if rurl == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer rdb.Close()

	const limit = 3
	// Per-test client_id is unique; flush any leftover key first.
	clientID := "rl-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	// Wrap the inner handler so it injects CtxClientID; this mirrors
	// what the real router does after cache auth.
	inner := RateLimit(rdb, limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(SetClientID(r.Context(), clientID))
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	success, fail := 0, 0
	for i := 0; i < limit+5; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/?_="+strconv.Itoa(i), nil)
		resp := mustDo(t, req)
		resp.Body.Close()
		switch resp.StatusCode {
		case 200:
			success++
		case 429:
			fail++
			if resp.Header.Get("Retry-After") == "" {
				t.Error("missing Retry-After on 429")
			}
		default:
			t.Errorf("unexpected status %d", resp.StatusCode)
		}
	}
	if fail == 0 {
		t.Errorf("expected some 429s (success=%d fail=%d)", success, fail)
	}
}

// silence unused import in case the test skips
var _ = redis.Nil

func TestLogging_RecordsFields(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	// Debug level: successful GETs log at Debug, writes/errors at Info.
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := Logging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
	}))
	srv := httptest.NewServer(RequestID()(h))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"path":"/"`) {
		t.Errorf("log missing path: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":201`) {
		t.Errorf("log missing status: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Errorf("successful GET should log at DEBUG, got: %s", buf.String())
	}
}

func TestLogging_ErrorsStayInfo(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := Logging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	srv := httptest.NewServer(RequestID()(h))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if !strings.Contains(buf.String(), `"status":500`) {
		t.Errorf("error responses must stay at INFO, got: %s", buf.String())
	}
}

func TestSetClientID(t *testing.T) {
	ctx := SetClientID(context.Background(), "abc-123")
	if v, _ := ctx.Value(CtxClientID).(string); v != "abc-123" {
		t.Errorf("ctx value=%q", v)
	}
}
