// End-to-end smoke: fake Telegram API + fake LLM + real DB through
// the manager→agent→tool→reply loop (per-bot poll path). Run with:
//
//	DATABASE_URL=... DATABASE_ADMIN_URL=... REDIS_URL=... \
//	  go test -run TestManager_EndToEndBook ./internal/telegram/
//
// DONE: T3 (manager e2e)
package telegram

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	_ "github.com/davinakmalyasha/agents-proxy/AgentsTools/CentrumBadminton"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/internal/tgvault"
	"google.golang.org/adk/v2/session"
)

func TestManager_EndToEndBook(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	rurl := os.Getenv("REDIS_URL")
	if dsn == "" || adminDSN == "" || rurl == "" {
		t.Skip("integration env not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })

	// Seed: client + facility + bot row (owner role, chat allowlisted).
	tenant := "smoke-tg"
	for _, stmt := range []string{
		`DELETE FROM agent_reminders WHERE tenant_id='smoke-tg'`,
		`DELETE FROM sports_bookings WHERE tenant_id='smoke-tg'`,
		`DELETE FROM sports_pricing_rules WHERE tenant_id='smoke-tg'`,
		`DELETE FROM sports_facilities WHERE tenant_id='smoke-tg'`,
		`DELETE FROM telegram_bots WHERE tenant_id='smoke-tg'`,
		`DELETE FROM clients WHERE tenant_id='smoke-tg'`,
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var facID string
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ('smoke-tg','smoke-tg','smoke-tg-hash',1000000,'active','sports',ARRAY['records','facilities','bookings','telegram'])`); err != nil {
		t.Fatal(err)
	}
	if err := adminPool.QueryRow(ctx,
		`INSERT INTO sports_facilities (tenant_id, name, type, price_per_hour, open_time, close_time)
		 VALUES ('smoke-tg','Court 3','badminton',50000,'06:00','22:00') RETURNING id::text`).Scan(&facID); err != nil {
		t.Fatal(err)
	}
	// book_slot prices from the tenant's pricing rule, never from the
	// model: without a covering rule the tool refuses, so seed one.
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO sports_pricing_rules
		   (tenant_id, valid_from, open_time, close_time, day_start, day_end,
		    price_day, price_evening, is_active)
		 VALUES ('smoke-tg', CURRENT_DATE, '06:00','22:00','06:00','22:00',50000,75000,true)`); err != nil {
		t.Fatal(err)
	}
	vaultKey := make([]byte, 32)
	if _, err := rand.Read(vaultKey); err != nil {
		t.Fatal(err)
	}
	enc, err := tgvault.Encrypt(vaultKey, []byte("TEST"))
	if err != nil {
		t.Fatal(err)
	}
	var botID string
	if err := adminPool.QueryRow(ctx,
		`INSERT INTO telegram_bots (tenant_id, name, role, token_enc, webhook_secret, allow_chat_ids)
		 VALUES ('smoke-tg','Owner','owner',$1,'shook',ARRAY[424242]) RETURNING id::text`,
		enc).Scan(&botID); err != nil {
		t.Fatal(err)
	}

	// Fake Telegram API: one message then empty; record sends.
	var sent []map[string]any
	var polls int
	fakeTg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTEST/getUpdates":
			polls++
			if polls == 1 {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"chat":{"id":424242},"text":"book Court 3 tomorrow 19:00-20:00 for Alice"}}]}`))
			} else {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			}
		case "/botTEST/sendMessage", "/botTEST/deleteWebhook":
			var b map[string]any
			if r.URL.Path == "/botTEST/sendMessage" {
				_ = json.NewDecoder(r.Body).Decode(&b)
				sent = append(sent, b)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer fakeTg.Close()
	SetAPIBase(fakeTg.URL + "/bot%s")

	// Fake LLM speaking real chat-completions: the first request gets a
	// book_slot call, every later one the narration. (The previous fake
	// spoke the retired {"tool":...} text protocol, which no loop has
	// honoured since function calling replaced it.)
	tomorrow := time.Now().Add(24 * time.Hour).Format("2006-01-02")
	callArgs, _ := json.Marshal(map[string]any{
		"facility_id": facID, "date": tomorrow,
		"start_time": "19:00", "end_time": "20:00", "customer_name": "Alice",
	})
	chatCompletion := func(inner string) string {
		return `{"id":"chatcmpl-e2e","object":"chat.completion","created":1700000000,` +
			`"model":"mock-model","choices":[{"index":0,` + inner + `}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	}
	callBody := chatCompletion(`"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"book_slot",` +
		`"arguments":` + strconv.Quote(string(callArgs)) + `}}]},` +
		`"finish_reason":"tool_calls"`)
	finalBody := chatCompletion(`"message":{"role":"assistant","content":` +
		strconv.Quote("Booked Court 3 for Alice on "+tomorrow+" 19:00-20:00.") + `},` +
		`"finish_reason":"stop"`)
	var llmCalls int
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls++
		w.Header().Set("Content-Type", "application/json")
		if llmCalls == 1 {
			_, _ = w.Write([]byte(callBody))
			return
		}
		_, _ = w.Write([]byte(finalBody))
	}))
	defer fakeLLM.Close()

	// Sessions are in-memory here: the loop is what's under test, and
	// this leaves no session rows behind. Bookings assert separately.
	adkRunner, err := adkrunner.New(adkrunner.Config{
		Pool:      pool,
		BaseURL:   fakeLLM.URL,
		APIKey:    "sk-test",
		Model:     "mock-model",
		MaxTokens: 512,
		Sessions:  session.InMemoryService(),
	})
	if err != nil {
		t.Fatalf("adk runner: %v", err)
	}

	mgr := NewManager(adminPool, adkRunner, vaultKey, "", true, 0)
	defer mgr.Close()
	mgr.Sync(ctx)
	if mgr.Count() != 1 {
		t.Fatalf("bots=%d want 1", mgr.Count())
	}

	deadline := time.Now().Add(15 * time.Second)
	for len(sent) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if len(sent) == 0 {
		t.Fatal("bot sent no reply")
	}
	reply, _ := sent[len(sent)-1]["text"].(string)
	t.Logf("bot reply: %s", reply)

	var n int
	if err := records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sports_bookings WHERE tenant_id='smoke-tg'`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("bookings=%d want 1", n)
	}

	// Cleanup.
	for _, stmt := range []string{
		`DELETE FROM sports_bookings WHERE tenant_id='smoke-tg'`,
		`DELETE FROM sports_pricing_rules WHERE tenant_id='smoke-tg'`,
		`DELETE FROM sports_facilities WHERE tenant_id='smoke-tg'`,
		`DELETE FROM telegram_bots WHERE tenant_id='smoke-tg'`,
		`DELETE FROM clients WHERE tenant_id='smoke-tg'`,
	} {
		_, _ = adminPool.Exec(ctx, stmt)
	}
}
