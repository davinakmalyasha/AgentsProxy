package agentstools

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WibNow returns now in WIB (UTC+7, no DST). FixedZone avoids tzdata
// dependency on Windows containers.
//
// TIME POLICY (single source of truth):
//   - Business-day buckets (caps keys, availability default date, expense
//     spent_at default, series "future", report months, prompt date) use
//     WIB via this function. Server-local midnight would bill the wrong
//     day for seven hours a day on a UTC host.
//   - Technical buckets stay UTC by explicit decision, not by accident:
//     quota month + resetAt wire format (quota.go), usage month
//     (usage/store.go), rate-limit minute bucket (ratelimit.go).
//     Migrating those to WIB needs double-write + reconciliation, because
//     quota months and report months would otherwise disagree on the
//     month boundary.
func WibNow() time.Time {
	return time.Now().In(WibLoc())
}

// WibLoc is the WIB location for ParseInLocation.
func WibLoc() *time.Location {
	return time.FixedZone("WIB", 7*3600)
}

// DefaultMonthRange returns (firstOfMonth, firstOfNextMonth) unless
// both bounds are given.
func DefaultMonthRange(from, to string) (string, string) {
	if from != "" && to != "" {
		return from, to
	}
	now := WibNow()
	first := now.Format("2006-01") + "-01"
	next := now.AddDate(0, 1, 0).Format("2006-01-02")
	return first, next
}

// Basis is one range's money, both ways.
type Basis struct {
	Cash   float64 // payments, method=cash
	QRIS   float64 // payments, method=qris
	Other  float64 // payments, any other method (transfer/card/...)
	Booked float64 // COALESCE(override_price, price) over live bookings
	Kantin float64 // canteen sales
	Count  int     // payments counted
}

// Received is money actually taken: payments, plus canteen sales.
func (b Basis) Received() float64 { return b.Cash + b.QRIS + b.Other + b.Kantin }

// LoadBasis reads both bases for [from, to). Payment and expense dates are
// timestamptz and are compared in WIB, matching how they are entered;
// booking and expense dates are plain dates.
func LoadBasis(ctx context.Context, tx pgx.Tx, tenantID, from, to string) (Basis, error) {
	var b Basis
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount) FILTER (WHERE method='cash'),0),
		        COALESCE(SUM(amount) FILTER (WHERE method='qris'),0),
		        COALESCE(SUM(amount) FILTER (WHERE method NOT IN ('cash','qris')),0),
		        COUNT(*)
		   FROM sports_payments
		  WHERE tenant_id=$1
		    AND deleted_at IS NULL
		    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
		    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3::date`,
		tenantID, from, to).Scan(&b.Cash, &b.QRIS, &b.Other, &b.Count); err != nil {
		return b, err
	}
	// Committed value, excluding slots that will never be charged.
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(COALESCE(override_price, price)),0)
		   FROM sports_bookings
		  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
		    AND status IN ('confirmed','checked_in','completed')`,
		tenantID, from, to).Scan(&b.Booked); err != nil {
		return b, err
	}
	// voided_at IS NULL: a voided sale is a correction, not revenue. Without
	// this the agent would keep reporting money that was taken back.
	return b, tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(total),0) FROM sports_kantin_sales
		  WHERE tenant_id=$1
		    AND voided_at IS NULL
		    AND (created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
		    AND (created_at AT TIME ZONE 'Asia/Jakarta')::date <  $3::date`,
		tenantID, from, to).Scan(&b.Kantin)
}

// Describe renders the basis in one sentence so every tool phrases the
// same two numbers the same way.
func (b Basis) Describe() string {
	return fmt.Sprintf("terima %.0f (cash %.0f, qris %.0f, kantin %.0f), teresan %.0f",
		b.Received(), b.Cash, b.QRIS, b.Kantin, b.Booked)
}
