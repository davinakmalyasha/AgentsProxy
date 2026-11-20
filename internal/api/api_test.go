package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/article"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/booking"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/menu"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/promo"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/review"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	_ "github.com/davinakmalyasha/agents-proxy/projects/centrum/agent/tools"
)

type harness struct {
	router   http.Handler
	pool     *pgxpool.Pool
	redis    *redis.Client
	mockLLM  *httptest.Server
	adminKey string
}

func mustReq(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *harness) close() {
	if h.pool != nil {
		h.pool.Close()
	}
	if h.redis != nil {
		h.redis.Close()
	}
	if h.mockLLM != nil {
		h.mockLLM.Close()
	}
}

func setup(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	rurl := os.Getenv("TEST_REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("TEST_DATABASE_URL / TEST_REDIS_URL not set — skipping integration test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	migPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, migPool); err != nil {
		migPool.Close()
		t.Fatal(err)
	}
	migPool.Close()

	// These tests don't exercise the RLS boundary — use the admin
	// DSN for the runtime pool. RLS is verified in business_test.go
	// via the `app` role harness.
	pool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	// Targeted cleanup — TRUNCATE would deadlock with parallel tests.
	// We delete every client the api tests might create. Other test
	// packages own their own rows; those run with separate harnesses.
	// Use the admin pool so RLS doesn't hide our own rows.
	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('alpha','bravo','gamma','x','x1','x2','salon-a','klinik-a','orig-probe','orig-probe-bad','auth-t')`); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM tenant_records WHERE tenant_id IN ('alpha','bravo','gamma','x','x1','x2','salon-a','klinik-a')`); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	// ADK conversation sessions from earlier runs (events cascade on
	// session delete). Same targeted style: only the test tenants.
	if _, err := adminPool.Exec(ctx, `DELETE FROM sessions WHERE app_name IN ('alpha','bravo','gamma','x','x1','x2','salon-a','klinik-a')`); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	rdb.FlushDB(ctx)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "mock",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "hi back"}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 20},
		})
	}))

	seedClient(t, ctx, pool, "alpha", "alpha", "tok-alpha", 100000, "active")
	seedClient(t, ctx, pool, "bravo", "bravo", "tok-bravo", 100000, "paused")

	adminKey := "admin-test-key-12345"
	cli := &cache.Client{DB: pool, RDB: rdb}
	llmCli := llm.New(mockLLM.URL, "sk-test", "mock-model", 1024)

	cfg := &config.Config{
		Port:               "0",
		Env:                "development",
		DatabaseURL:        dsn,
		RedisURL:           rurl,
		LLMBaseURL:         mockLLM.URL,
		LLMAPIKey:          "sk-test",
		LLMModel:           "mock-model",
		AdminAPIKey:        adminKey,
		MaxTokensPerReq:    1024,
		RateLimitPerMinute: 100000, // high so tests don't trip
		AllowOrigins:       []string{"*"},
	}
	router := NewRouter(Deps{
		Cfg:     cfg,
		Pool:    pool,
		Redis:   rdb,
		Cache:   cli,
		LLM:     llmCli,
		ADK:     testADKRunner(t, pool, mockLLM.URL),
		Auth:    &authapi.Auth{Pool: pool, RDB: rdb},
		Version: "test",
	})
	h := &harness{router: router, pool: pool, redis: rdb, mockLLM: mockLLM, adminKey: adminKey}
	t.Cleanup(h.close)
	return h
}

func seedClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, tenant, token string, quota int, status string) {
	t.Helper()
	hash := auth.HashToken(token)
	// Default to the full Phase 3 fnb feature set so existing
	// tests can hit /v1/fnb/menu and friends without extra setup.
	vertical := "fnb"
	features := []string{"records", "menu", "booking", "review", "article", "promo"}
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		name, tenant, hash, quota, status, vertical, features,
	); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ----- tests -----

func TestHealthz(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	m := readJSON(t, resp)
	if m["status"] != "ok" {
		t.Errorf("status=%v", m["status"])
	}
	if m["db"] != true || m["redis"] != true {
		t.Errorf("db=%v redis=%v", m["db"], m["redis"])
	}
}

func TestChat_MissingToken(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"client_token":"","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestChat_InvalidToken(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"client_token":"not-a-real-token","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestChat_PausedClient(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"client_token":"tok-bravo","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status=%d want 403", resp.StatusCode)
	}
}

func TestChat_Happy(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"client_token":"tok-alpha","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	m := readJSON(t, resp)
	if m["reply"] != "hi back" {
		t.Errorf("reply=%v", m["reply"])
	}
	// Usage row inserted for this client? Count rows for alpha
	// created in this test (>= 1). Global counts are unreliable
	// because parallel test packages share the table.
	var n int
	h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage u JOIN clients c ON c.id = u.client_id WHERE c.tenant_id='alpha' AND u.kind='chat'`,
	).Scan(&n)
	if n < 1 {
		t.Errorf("usage rows=%d want >= 1", n)
	}
}

func TestChat_HappyRedisCached(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	// First call populates ck cache.
	body := `{"client_token":"tok-alpha","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		resp := mustReq(t, req)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("call %d status=%d", i, resp.StatusCode)
		}
	}
	hash := auth.HashToken("tok-alpha")
	n, _ := h.redis.Exists(context.Background(), "ck:"+hash).Result()
	if n != 1 {
		t.Errorf("ck cache not populated; exists=%d", n)
	}
}

func TestChat_TooManyMessages(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	var msgs []string
	for i := 0; i < maxChatMessages+5; i++ {
		msgs = append(msgs, `{"role":"user","content":"hi"}`)
	}
	body := `{"client_token":"tok-alpha","messages":[` + strings.Join(msgs, ",") + `]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestChat_MessageTooLong(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	long := strings.Repeat("a", maxMessageRunes+1)
	body := `{"client_token":"tok-alpha","messages":[{"role":"user","content":"` + long + `"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestChat_UnknownModelFallsBackToDefault(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	// The mock LLM doesn't care about the model name; the point is that
	// an unlisted model must NOT be forwarded verbatim (cost/safety).
	body := `{"client_token":"tok-alpha","model":"gpt-4-ultra","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	// The recorded usage model must be the default, not the user string.
	var model string
	h.pool.QueryRow(context.Background(),
		`SELECT model FROM usage u JOIN clients c ON c.id=u.client_id
		  WHERE c.tenant_id='alpha' AND u.kind='chat' ORDER BY u.created_at DESC LIMIT 1`,
	).Scan(&model)
	if model != "mock-model" {
		t.Errorf("recorded model=%q want default mock-model", model)
	}
}

func TestSanitizeModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "mock-model"},
		{"deepseek-chat", "deepseek-chat"},
		{"DeepSeek-Chat", "deepseek-chat"},
		{"mimo", "mimo"},
		{"command-code", "command-code"},
		{"gpt-4-ultra", "mock-model"},
		{"", "mock-model"},
	}
	for _, c := range cases {
		if got := sanitizeModel(c.in, "mock-model"); got != c.want {
			t.Errorf("sanitizeModel(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// seedStaffUser creates an owner under tenant 'alpha' + a valid session.
func seedStaffUser(t *testing.T, h *harness) string {
	t.Helper()
	ctx := context.Background()
	bcryptHash := "$2a$10$C6UzMDM.H6dfI/f/IKcEeO7CZp1DlVwH7nK2IYwQmYkYQ9Xk2bXkK" // placeholder, never verified in tests
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO sports_users (tenant_id, email, password_hash, role, full_name)
		 VALUES ('alpha','owner@test.dev',$1,'owner','Owner Test')
		 ON CONFLICT (tenant_id, email) DO NOTHING`, bcryptHash); err != nil {
		t.Fatal(err)
	}
	var uid string
	if err := h.pool.QueryRow(ctx,
		`SELECT id::text FROM sports_users WHERE tenant_id='alpha' AND email='owner@test.dev'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := authapi.GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	// tenant_id is NOT NULL as of migration 032, so name it. This runs on
	// the test's superuser pool; the runtime role cannot insert here at all.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO sports_sessions (tenant_id, user_id, token_hash, expires_at)
		 SELECT tenant_id, $1, $2, now() + interval '1 hour' FROM sports_users WHERE id = $1`,
		uid, hash); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAgentClientChat_OversizedBodyRejected(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	big := strings.Repeat("a", 1<<20+1)
	body := `{"message":"` + big + `","tenant_id":"alpha"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agent/client-chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestAgentClientChat_LongMessageRejected(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	long := strings.Repeat("a", maxMessageRunes+1)
	body := `{"message":"` + long + `","tenant_id":"alpha"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agent/client-chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

// testADKRunner wires the cutover loop for API tests: live Postgres
// sessions (migration 041 applies in setup), mock-LLM model. The mock
// answers prose with no tool calls, so these tests prove the ADK path
// serves traffic; tool-calling through the loop is covered in adkrunner
// and by live verification.
func testADKRunner(t *testing.T, pool *pgxpool.Pool, llmURL string) *adkrunner.Runner {
	t.Helper()
	sessSvc, err := adkrunner.NewSessionService(pool)
	if err != nil {
		t.Fatalf("session service: %v", err)
	}
	r, err := adkrunner.New(adkrunner.Config{
		Pool:      pool,
		BaseURL:   llmURL,
		APIKey:    "sk-test",
		Model:     "mock-model",
		MaxTokens: 1024,
		Sessions:  sessSvc,
	})
	if err != nil {
		t.Fatalf("adk runner: %v", err)
	}
	return r
}

// TestAgentClientChat_ADKServesReply posts a short greeting through the
// migrated endpoint and expects the mock model's prose back. This is the
// first test that executes the ADK loop inside the HTTP stack.
func TestAgentClientChat_ADKServesReply(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"message":"halo, jam berapa buka?","tenant_id":"alpha"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agent/client-chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var got struct {
		Reply string `json:"reply"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Role != "client" {
		t.Errorf("role=%q want client", got.Role)
	}
	if got.Reply != "hi back" {
		t.Errorf("reply=%q want the mock model's prose", got.Reply)
	}
}

func TestAgentChat_OversizedBodyRejected(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	tok := seedStaffUser(t, h)
	big := strings.Repeat("a", 1<<20+1)
	body := `{"message":"` + big + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agent/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Token", tok)
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestChat_QuotaExceeded(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	// Seed usage that exceeds the alpha client's quota of 100000.
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO usage (client_id, tokens_in, tokens_out, model, kind)
		 SELECT id, 60000, 50000, 'mock-model', 'chat' FROM clients WHERE tenant_id='alpha'`,
	); err != nil {
		t.Fatal(err)
	}
	// Clear cached quota so SUM recomputes.
	h.redis.Del(context.Background(), "quota:"+lookupClientID(t, h.pool, "alpha")+":"+time.Now().UTC().Format("200601"))

	body := `{"client_token":"tok-alpha","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("missing Retry-After on 429")
	}
	m := readJSON(t, resp)
	if _, ok := m["resetAt"]; !ok {
		t.Errorf("missing resetAt: %+v", m)
	}
}

func TestAdmin_Create_WrongAuth(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(`{"name":"x","tenant_id":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestAdmin_Create_HappyAndPatch(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	// Create.
	body := `{"name":"gamma","tenant_id":"gamma","monthly_quota_tokens":5000}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	m := readJSON(t, resp)
	tok, _ := m["token"].(string)
	if tok == "" {
		t.Fatal("token empty in response")
	}
	// Token is hex 64.
	if len(tok) != 64 {
		t.Errorf("token len=%d", len(tok))
	}
	id, _ := m["id"].(string)
	// DB stores hash, not raw.
	var storedHash string
	h.pool.QueryRow(context.Background(), `SELECT token_hash FROM clients WHERE id = $1::uuid`, id).Scan(&storedHash)
	if storedHash == tok {
		t.Error("DB stored raw token, not hash")
	}
	if storedHash != auth.HashToken(tok) {
		t.Error("DB hash does not match HashToken(raw)")
	}
	// PATCH to paused.
	patch := `{"status":"paused"}`
	req2, _ := http.NewRequest("PATCH", srv.URL+"/v1/admin/clients/"+id, bytes.NewReader([]byte(patch)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp2 := mustReq(t, req2)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("PATCH status=%d", resp2.StatusCode)
	}
	// Subsequent /v1/chat with that token should 403.
	req3, _ := http.NewRequest("POST", srv.URL+"/v1/chat", bytes.NewReader([]byte(`{"client_token":"`+tok+`","messages":[{"role":"user","content":"hi"}]}`)))
	req3.Header.Set("Content-Type", "application/json")
	resp3 := mustReq(t, req3)
	resp3.Body.Close()
	if resp3.StatusCode != 403 {
		t.Errorf("paused chat status=%d want 403", resp3.StatusCode)
	}
}

func TestAdmin_Usage(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/usage", nil)
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

// TestAdmin_ContractBump: canary bump happy path + every rejection.
// Rollback is just a bump back.
func TestAdmin_ContractBump(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	alphaID := lookupClientID(t, h.pool, "alpha")
	bump := func(id, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients/"+id+"/contract", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		resp := mustReq(t, req)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(raw)
	}
	// Happy: bump alpha bookings to v1 (current catalog max).
	if code, raw := bump(alphaID, `{"feature":"bookings","version":1}`); code != 200 {
		t.Fatalf("bump status=%d body=%s want 200", code, raw)
	}
	var tenant string
	var ver int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT tenant_id, version FROM tenant_schema_contracts WHERE feature='bookings' AND tenant_id='alpha'`).Scan(&tenant, &ver); err != nil {
		t.Fatalf("contract row missing: %v", err)
	}
	if ver != 1 {
		t.Errorf("version=%d want 1", ver)
	}
	for name, tc := range map[string]struct {
		id   string
		body string
		want int
	}{
		"unknown feature":  {alphaID, `{"feature":"nope","version":1}`, 400},
		"ahead of catalog": {alphaID, `{"feature":"bookings","version":99}`, 400},
		"version zero":     {alphaID, `{"feature":"bookings","version":0}`, 400},
		"empty feature":    {alphaID, `{"feature":"","version":1}`, 400},
		"bad json":         {alphaID, `not-json`, 400},
		"unknown tenant":   {"00000000-0000-0000-0000-000000000000", `{"feature":"bookings","version":1}`, 404},
		"bad id":           {"not-a-uuid", `{"feature":"bookings","version":1}`, 400},
	} {
		if code, raw := bump(tc.id, tc.body); code != tc.want {
			t.Errorf("%s: status=%d body=%s want %d", name, code, raw, tc.want)
		}
	}
}

// TestAdmin_TenantHealthAndReload: quarantine surface reports and the
// reload path re-validates a healthy tenant back to serving.
func TestAdmin_TenantHealthAndReload(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	adminH := func(method, path, body string) *http.Response {
		var req *http.Request
		if body != "" {
			req, _ = http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req, _ = http.NewRequest(method, srv.URL+path, nil)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		return mustReq(t, req)
	}
	resp := adminH("GET", "/v1/admin/tenant-health", "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status=%d body=%s", resp.StatusCode, raw)
	}
	// Reload the healthy embedded tenant: exercises LoadSingle + Validate
	// + tool proof + hot-swap for real.
	resp2 := adminH("POST", "/v1/admin/tenant-health/reload", `{"tenant_id":"centrum"}`)
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("reload status=%d body=%s", resp2.StatusCode, raw2)
	}
	// Unknown slug → 400, never quarantine-by-typo.
	resp3 := adminH("POST", "/v1/admin/tenant-health/reload", `{"tenant_id":"nope"}`)
	raw3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 400 {
		t.Errorf("reload unknown status=%d body=%s want 400", resp3.StatusCode, raw3)
	}
}

// TestAdmin_UsageRejectsBadDates: garbage from/to must 400, never silently
// unfilter into a lifetime report. Inverted range must 400 too.
func TestAdmin_UsageRejectsBadDates(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	for _, q := range []string{
		"?from=garbage",
		"?to=2026-13-99",
		"?from=2026-03-01T00:00:00Z&to=2026-01-01T00:00:00Z",
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/usage"+q, nil)
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		resp := mustReq(t, req)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("usage%s status=%d body=%s want 400", q, resp.StatusCode, raw)
		}
	}
}

// TestAdmin_CreateRejectsBadShape: malformed tenant ids and out-of-range
// quotas must 400, never become RLS key material or silent unlimited.
func TestAdmin_CreateRejectsBadShape(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	bodies := []string{
		`{"name":"x","tenant_id":"CENTRUM"}`,
		`{"name":"x","tenant_id":"has space"}`,
		`{"name":"x","tenant_id":"a' OR 1=1"}`,
		`{"name":"x","tenant_id":"t","monthly_quota_tokens":-1}`,
		`{"name":"x","tenant_id":"t","monthly_quota_tokens":100000001}`,
	}
	for i, b := range bodies {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		resp := mustReq(t, req)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("case %d status=%d body=%s want 400", i, resp.StatusCode, raw)
		}
	}
}

func lookupClientID(t *testing.T, pool *pgxpool.Pool, tenant string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM clients WHERE tenant_id=$1`, tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ---- Phase 3 feature tests ----

// TestFeatureMenu_Happy: create a menu item, list, get-one.
func TestFeatureMenu_Happy(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	// create
	req, _ := http.NewRequest("POST", srv.URL+"/v1/fnb/menu",
		bytes.NewReader([]byte(`{"title":"Nasi Goreng","payload":{"name":"Nasi Goreng","price":15000}}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Token", "tok-alpha")
	resp := mustReq(t, req)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("menu create status=%d body=%s", resp.StatusCode, raw)
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("menu create: no id")
	}
	// list — filter by our own title to ignore cross-tenant data
	// (api tests use the admin pool, which bypasses RLS).
	req, _ = http.NewRequest("GET", srv.URL+"/v1/fnb/menu?q=Nasi+Goreng", nil)
	req.Header.Set("X-Client-Token", "tok-alpha")
	resp = mustReq(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("menu list status=%d", resp.StatusCode)
	}
	m = readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) < 1 {
		t.Errorf("menu list items=%d want >= 1", len(items))
	}
}

// TestFeatureMenu_Validation: missing name → 400.
func TestFeatureMenu_Validation(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/fnb/menu",
		bytes.NewReader([]byte(`{"title":"x","payload":{"name":"","price":-1}}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Token", "tok-alpha")
	resp := mustReq(t, req)
	if resp.StatusCode != 400 {
		t.Errorf("menu bad payload status=%d want 400", resp.StatusCode)
	}
}

// TestFeatureMenu_KindForcedServerSide: client tries to write
// kind=service; the adapter must override to menu_item.
func TestFeatureMenu_KindForcedServerSide(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/fnb/menu",
		bytes.NewReader([]byte(`{"kind":"service","title":"x","payload":{"name":"x","price":1}}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Token", "tok-alpha")
	resp := mustReq(t, req)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// GET it back — the kind must be menu_item regardless of what
	// the client sent.
	req, _ = http.NewRequest("GET", srv.URL+"/v1/fnb/menu/"+id, nil)
	req.Header.Set("X-Client-Token", "tok-alpha")
	resp = mustReq(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("get status=%d", resp.StatusCode)
	}
	m2 := readJSON(t, resp)
	if m2["kind"] != "menu_item" {
		t.Errorf("kind=%v want menu_item", m2["kind"])
	}
}

// TestFeatureGate_BlocksWhenRemoved: PATCH features to remove menu,
// then POST to /v1/fnb/menu must 403.
func TestFeatureGate_BlocksWhenRemoved(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	// Find alpha's id.
	row := h.pool.QueryRow(context.Background(), `SELECT id::text FROM clients WHERE tenant_id='alpha'`)
	var id string
	if err := row.Scan(&id); err != nil {
		t.Fatal(err)
	}
	// PATCH features to drop menu.
	body := `{"features":["records","booking"]}`
	req, _ := http.NewRequest("PATCH", srv.URL+"/v1/admin/clients/"+id, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("patch status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	// Now try POST /v1/fnb/menu.
	req, _ = http.NewRequest("POST", srv.URL+"/v1/fnb/menu",
		bytes.NewReader([]byte(`{"name":"x","price":1,"payload":{}}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Token", "tok-alpha")
	resp = mustReq(t, req)
	if resp.StatusCode != 403 {
		t.Errorf("menu without feature status=%d want 403", resp.StatusCode)
	}
}

// TestAdmin_CreateWithVerticalAndFeatures: default vertical + features.
func TestAdmin_CreateWithVerticalAndFeatures(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"name":"salonA","tenant_id":"salon-a"}` // vertical defaults to fnb, features default
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	m := readJSON(t, resp)
	if m["vertical"] != "fnb" {
		t.Errorf("vertical=%v want fnb", m["vertical"])
	}
	feats, _ := m["features"].([]any)
	if len(feats) == 0 {
		t.Errorf("features empty, want default set")
	}
}

// TestAdmin_CreateWithExplicitVerticalAndFeatures: pass vertical=klinik, features=[records, booking, review]
func TestAdmin_CreateWithExplicitVerticalAndFeatures(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"name":"klinikA","tenant_id":"klinik-a","vertical":"klinik","features":["records","booking","review"]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	m := readJSON(t, resp)
	if m["vertical"] != "klinik" {
		t.Errorf("vertical=%v want klinik", m["vertical"])
	}
}

// TestAdmin_CreateUnknownFeature: 400.
func TestAdmin_CreateUnknownFeature(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"name":"x","tenant_id":"x1","vertical":"fnb","features":["nosuch"]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

// TestAdmin_List: returns the seeded clients.
func TestAdmin_List(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/clients", nil)
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var arr []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		t.Fatal(err)
	}
	// Only check our two seeded clients (alpha, bravo) — other
	// clients from other packages' seed data may exist.
	mine := 0
	for _, c := range arr {
		tenantID, _ := c["tenant_id"].(string)
		if tenantID != "alpha" && tenantID != "bravo" {
			continue
		}
		mine++
		if c["vertical"] != "fnb" {
			t.Errorf("%s vertical=%v want fnb", tenantID, c["vertical"])
		}
		feats, _ := c["features"].([]any)
		if len(feats) == 0 {
			t.Errorf("%s features empty", tenantID)
		}
	}
	if mine != 2 {
		t.Errorf("found %d of my clients, want 2", mine)
	}
}

// TestAdmin_UnknownVertical: 400.
func TestAdmin_UnknownVertical(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	body := `{"name":"x","tenant_id":"x2","vertical":"nosuch"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp := mustReq(t, req)
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}
