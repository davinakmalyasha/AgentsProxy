package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestNormalizePhone pins migration 065 at both layers: the pure
// function, and the storage-boundary triggers (customers + bookings).
func TestNormalizePhone(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"0812-3456-7890":    "+6281234567890",
		"62 812 3456 7890":  "+6281234567890",
		"+62 812-3456-7890": "+6281234567890",
		"81234567890":       "+6281234567890",
		"+65 9123 4567":     "+6591234567",
		"":                  "",
	}
	for in, want := range cases {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT private.normalize_phone($1)`, in).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("normalize_phone(%q) = %q, want %q", in, got, want)
		}
	}
	var nullOut *string
	if err := pool.QueryRow(ctx, `SELECT private.normalize_phone(NULL)`).Scan(&nullOut); err != nil {
		t.Fatal(err)
	}
	if nullOut != nil {
		t.Errorf("normalize_phone(NULL) = %v, want NULL", *nullOut)
	}

	// Triggers canonicalize on write for every writer.
	const tenant = "phone65-test"
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cpool, err := NewPool(cctx, adminDSN)
		if err != nil {
			return
		}
		defer cpool.Close()
		cpool.Exec(cctx, `DELETE FROM sports_bookings WHERE tenant_id = $1`, tenant)
		cpool.Exec(cctx, `DELETE FROM sports_customers WHERE tenant_id = $1`, tenant)
		cpool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('ph65', $1, 'ph65-hash', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}

	var customerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sports_customers (tenant_id, name, phone)
		 VALUES ($1, 'Budi', '0812-3456-7890') RETURNING id::text`, tenant).Scan(&customerID); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT phone FROM sports_customers WHERE id = $1::uuid`, customerID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "+6281234567890" {
		t.Errorf("customer phone stored %q, want normalized", stored)
	}

	// Mixed-format duplicate must trip the existing unique constraint.
	_, err = pool.Exec(ctx,
		`INSERT INTO sports_customers (tenant_id, name, phone)
		 VALUES ($1, 'Budi 2', '6281234567890')`, tenant)
	if err == nil {
		t.Error("mixed-format duplicate customer accepted, want unique violation")
	}

	// Bookings normalize the denormalized copy too (the legacy join is
	// string equality against customers.phone).
	var facilityID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sports_facilities (tenant_id, name, type, price_per_hour)
		 VALUES ($1, 'Court 65', 'badminton', 50000) RETURNING id::text`, tenant).Scan(&facilityID); err != nil {
		t.Fatal(err)
	}
	var bookingPhone string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, price)
		 VALUES ($1, $2::uuid, '2026-10-20', '18:00', '19:00', 'Budi', '+62 812 3456 7890', 50000)
		 RETURNING customer_phone`, tenant, facilityID).Scan(&bookingPhone); err != nil {
		t.Fatal(err)
	}
	if bookingPhone != "+6281234567890" {
		t.Errorf("booking customer_phone stored %q, want normalized", bookingPhone)
	}
}
