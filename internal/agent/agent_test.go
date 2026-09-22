// Agent loop tests using a fake LLM server. No real LLM call.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// fakeLLM replies with a canned sequence of JSON responses, one per
// chat request. If responses[i] contains the literal placeholder
// "__FAC__", it is replaced with the realFacilityID (set by the
// test after seeding) so the fake model calls tools with a valid id.
func fakeLLM(t *testing.T, responses []string, realFacilityID *string) *httptest.Server {
	t.Helper()
	var i int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i >= len(responses) {
			t.Fatalf("LLM called more than %d times", len(responses))
		}
		resp := responses[i]
		i++
		if realFacilityID != nil {
			resp = strings.ReplaceAll(resp, "__FAC__", *realFacilityID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + mustJSON(resp) + `}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func agentHarness(t *testing.T, responses []string, realFacilityID *string) (*Agent, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	llmSrv := fakeLLM(t, responses, realFacilityID)
	t.Cleanup(llmSrv.Close)
	llmCli := llm.New(llmSrv.URL, "sk-test", "mock-model", 512)

	dsn := os.Getenv("DATABASE_URL")
	rurl := os.Getenv("REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("DATABASE_URL / REDIS_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// Seed the test clients (needed for the FK from sports_* tables).
	// Use the admin DSN so RLS doesn't block the inserts.
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	for _, tenant := range []string{"agt-test", "agt-test2"} {
		for _, stmt := range []string{
			`DELETE FROM sports_bookings WHERE tenant_id=$1`,
			`DELETE FROM sports_facilities WHERE tenant_id=$1`,
			`DELETE FROM agent_reminders WHERE tenant_id=$1`,
			`DELETE FROM agent_staff_payments WHERE tenant_id=$1`,
			`DELETE FROM clients WHERE tenant_id=$1`,
		} {
			if _, err := adminPool.Exec(ctx, stmt, tenant); err != nil {
				t.Fatal(err)
			}
		}
		hash := "hash-" + tenant // placeholder, unused by these tests
		if _, err := adminPool.Exec(ctx,
			`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
			 VALUES ($1,$2,$3,1000,'active','sports',ARRAY['records','telegram'])`,
			tenant, tenant, hash); err != nil {
			t.Fatal(err)
		}
	}

	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })

	ts := NewToolSet(pool)
	hist := &History{RDB: rdb}
	return New(llmCli, ts, hist), pool, rdb
}

func TestAgent_BooksSlot(t *testing.T) {
	var facID string
	// LLM: first call → tool call (with real facility id); second
	// call (after tool result) → answer.
	a, pool, _ := agentHarness(t, []string{
		`{"tool":"book_slot","args":{"facility_id":"__FAC__","date":"2026-09-10","start_time":"19:00","end_time":"20:00","customer_name":"Alice"}}`,
		`{"answer":"Done! Court booked for Alice on 2026-09-10 19:00-20:00."}`,
	}, &facID)
	// Seed a facility for tenant 'agt-test' (inside a tenant tx so
	// RLS allows the insert — the app role has no GUC by default).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := records.WithTenantTx(ctx, pool, "agt-test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sports_facilities (tenant_id, name, type, price_per_hour) VALUES ('agt-test','Court 3','badminton',50000) RETURNING id::text`,
		).Scan(&facID)
	})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := a.Run(ctx, "agt-test", "sports", "Owner", "book Court 3 tomorrow 19:00")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "Court") {
		t.Errorf("reply=%q want mention of court", reply)
	}
	// The tool executed against the real DB: a booking row exists
	// (count inside a tenant tx so RLS lets us see our own rows).
	var n int
	err = records.WithTenantTx(ctx, pool, "agt-test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM sports_bookings WHERE tenant_id='agt-test'`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("bookings=%d want 1", n)
	}
}

func TestAgent_DoubleBookErrorPropagates(t *testing.T) {
	var facID string
	// LLM: call book_slot → tool returns "slot already booked" error →
	// LLM must see it and answer with the alternative offer.
	a, pool, _ := agentHarness(t, []string{
		`{"tool":"book_slot","args":{"facility_id":"__FAC__","date":"2026-09-10","start_time":"19:00","end_time":"20:00","customer_name":"Bob"}}`,
		`{"answer":"That slot is already booked. Want 20:00-21:00 instead?"}`,
	}, &facID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Seed a facility + an existing conflicting booking for tenant
	// 'agt-test2' (inside tenant txs so RLS allows the inserts).
	err := records.WithTenantTx(ctx, pool, "agt-test2", func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO sports_facilities (tenant_id, name, type) VALUES ('agt-test2','Court 3','badminton') RETURNING id::text`,
		).Scan(&facID); err != nil {
			return err
		}
		// Pre-book 19:00-20:00 on 2026-09-10.
		_, err := tx.Exec(ctx,
			`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name)
			 VALUES ('agt-test2',$1::uuid,'2026-09-10','19:00','20:00','Alice')`, facID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := a.Run(ctx, "agt-test2", "sports", "Owner", "book Court 3 tomorrow 19:00")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The fake LLM's answer should surface (the tool error fed back).
	if !strings.Contains(reply, "booked") && !strings.Contains(reply, "alternative") {
		t.Errorf("reply=%q", reply)
	}
	// Exactly one booking remains (Alice's; Bob's was rejected).
	var n int
	err = records.WithTenantTx(ctx, pool, "agt-test2", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM sports_bookings WHERE tenant_id='agt-test2'`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("bookings=%d want 1 (double-book must be rejected)", n)
	}
}

func TestParseReply(t *testing.T) {
	call, ans, ok := parseReply(`{"tool":"book_slot","args":{"x":1}}`)
	if !ok || call.Tool != "book_slot" || ans != "" {
		t.Errorf("tool parse: %+v %q %v", call, ans, ok)
	}
	_, ans2, ok2 := parseReply(`{"answer":"hello"}`)
	if !ok2 || ans2 != "hello" {
		t.Errorf("answer parse: %q %v", ans2, ok2)
	}
	// Non-JSON → not a structured reply.
	_, _, ok3 := parseReply("sure, let me check")
	if ok3 {
		t.Errorf("plain text should not parse as structured")
	}
}
