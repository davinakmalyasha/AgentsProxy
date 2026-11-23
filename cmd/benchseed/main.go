// Command benchseed fills a fresh local database with a realistic Centrum
// tenant for the performance harness.
//
// It is intentionally not a fixture loader for tests: tests own their cloned
// schemas. This command runs against a disposable agents_proxy_bench_* DB,
// creates the real tenant/client rows, and seeds enough rows that list
// queries, availability, and login are measured against representative data.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchseed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		adminURL    = fs.String("admin-url", os.Getenv("DATABASE_ADMIN_URL"), "admin DSN for the bench database")
		tenant      = fs.String("tenant", "centrum", "tenant slug")
		clientToken = fs.String("client-token", "bench-client-token", "raw client token to seed")
		staffToken  = fs.String("staff-token", "bench-staff-token", "raw staff session token to seed")
		password    = fs.String("password", "bench-password", "staff password for the login scenario")
		bookings    = fs.Int("bookings", 500, "booking rows")
		records     = fs.Int("records", 500, "record rows")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *adminURL == "" {
		fmt.Fprintln(stderr, "benchseed: -admin-url or DATABASE_ADMIN_URL is required")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := seed(ctx, *adminURL, *tenant, *clientToken, *staffToken, *password, *bookings, *records); err != nil {
		fmt.Fprintln(stderr, "benchseed:", err)
		return 1
	}
	fmt.Fprintf(stdout, "BENCH_TENANT=%q\nBENCH_CLIENT_TOKEN=%q\nBENCH_STAFF_TOKEN=%q\nBENCH_STAFF_EMAIL=%q\nBENCH_STAFF_PASSWORD=%q\n",
		*tenant, *clientToken, *staffToken, "bench-owner@example.test", *password)
	return 0
}

func seed(ctx context.Context, adminURL, tenant, clientToken, staffToken, password string, bookings, records int) error {
	pool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}

	features := []string{
		"records", "availability", "bookings", "courtblocks", "customers",
		"expenses", "facilities", "inventory", "kantin", "members",
		"payments", "pricing", "series", "users",
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Replace only this synthetic tenant. The database is disposable, but
	// making the seed repeatable is cheap and prevents a half-run from
	// producing confusing duplicates.
	if _, err := tx.Exec(ctx, `DELETE FROM public.clients WHERE tenant_id=$1`, tenant); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO public.clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Bench Centrum',$1,$2,100000000,'active','sports',$3,'project_centrum')`,
		tenant, auth.HashToken(clientToken), features); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO project_centrum.sports_users (tenant_id, email, password_hash, role, full_name)
		 VALUES ($1,'bench-owner@example.test',$2,'owner','Bench Owner')
		 RETURNING id::text`, tenant, string(hash)).Scan(&userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`SELECT private.staff_session_create($1, $2::uuid, $3, now() + interval '24 hours')`,
		tenant, userID, auth.HashToken(staffToken)); err != nil {
		return err
	}

	facilities := []struct {
		name, typ string
		price     int
	}{
		{"Court 1", "badminton", 50000}, {"Court 2", "badminton", 50000},
		{"Court 3", "badminton", 55000}, {"Court 4", "badminton", 55000},
		{"Court 5", "badminton", 60000}, {"Court 6", "badminton", 60000},
		{"Futsal Arena", "soccer", 150000}, {"Padel Court", "padel", 90000},
	}
	facilityIDs := make([]string, 0, len(facilities))
	for _, f := range facilities {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO project_centrum.sports_facilities
			   (tenant_id,name,type,capacity,price_per_hour,open_time,close_time)
			 VALUES ($1,$2,$3,4,$4,'06:00','23:00') RETURNING id::text`,
			tenant, f.name, f.typ, f.price).Scan(&id); err != nil {
			return err
		}
		facilityIDs = append(facilityIDs, id)
	}

	for i := 0; i < 12; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.sports_pricing_rules
			   (tenant_id, valid_from, open_time, close_time, day_start, day_end, price_day, price_evening)
			 VALUES ($1, current_date + $2::int, '06:00','23:00','06:00','17:00',50000,75000)`,
			tenant, i); err != nil {
			return err
		}
	}

	// 8 facilities × 12 one-hour slots per day gives 96 unique slots/day.
	// 500 bookings therefore span just over five days without violating the
	// exclusion constraint.
	for i := 0; i < bookings; i++ {
		facility := facilityIDs[i%len(facilityIDs)]
		slot := (i / len(facilityIDs)) % 12
		day := i / (len(facilityIDs) * 12)
		start := time.Date(2026, 1, 1, 8+slot, 0, 0, 0, time.UTC)
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.sports_bookings
			   (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status, price)
			 VALUES ($1,$2,(current_date + $3::int)::date,$4::time,($4::time + interval '1 hour')::time,$5,$6,'confirmed',50000)`,
			tenant, facility, day-3, start.Format("15:04"), fmt.Sprintf("Customer %d", i%200), fmt.Sprintf("0815%08d", i)); err != nil {
			return err
		}
	}

	for i := 0; i < 50; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.sports_court_blocks
			   (tenant_id, facility_id, block_date, start_time, end_time, reason)
			 VALUES ($1,$2,(current_date + $3::int)::date,'12:00','13:00','maintenance')`,
			tenant, facilityIDs[i%len(facilityIDs)], i%10); err != nil {
			return err
		}
	}

	for i := 0; i < records; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.tenant_records (tenant_id, vertical, kind, title, payload)
			 VALUES ($1,'sports','item',$2,'{}'::jsonb)`,
			tenant, fmt.Sprintf("Bench record %d", i)); err != nil {
			return err
		}
	}

	for i := 0; i < 100; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.sports_expenses (tenant_id, category, amount, note, spent_at)
			 VALUES ($1,'operasional',25000,$2,(current_date - $3::int)::date)`,
			tenant, fmt.Sprintf("Bench expense %d", i), i%30); err != nil {
			return err
		}
	}
	for i := 0; i < 100; i++ {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.sports_members (tenant_id, name, phone, balance)
			 VALUES ($1,$2,$3,100000)`,
			tenant, fmt.Sprintf("Member %d", i), fmt.Sprintf("0812%08d", i)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_centrum.sports_customers (tenant_id, name, phone)
			 VALUES ($1,$2,$3)`,
			tenant, fmt.Sprintf("Customer %d", i), fmt.Sprintf("0813%08d", i)); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}
