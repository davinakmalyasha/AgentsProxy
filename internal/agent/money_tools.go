// Phase 7: money-reporting tools ported from CentrumBadminton's Gemini
// assistant (net profit, range summary, top customers, closing report,
// kantin summary) + a per-tenant daily cost cap.
//
// DONE: Phase 7 (agent money tools) / STANDARDS A, E
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

// MoneyCap is the per-tenant daily LLM-call cap (Centrum's guard).
const MoneyCap = 150

// CapRDB is the Redis client used by the cost cap. Set once in
// main.go wiring; tests leave it nil (no cap).
var CapRDB *redis.Client

// CapKey returns the Redis key for a tenant's daily call count.
func CapKey(tenantID string) string {
	return "agcap:" + tenantID + ":" + time.Now().Format("2006-01-02")
}

// CostCapAllowed checks (and increments) the per-tenant daily cap.
// Returns false when the tenant has hit the cap. Legacy global check.
func CostCapAllowed(ctx context.Context, tenantID string) bool {
	return CostCapAllowedForRole(ctx, tenantID, "owner")
}

// CostCapAllowedForRole checks per-tenant+role daily cap (20/80/150). Single LLM_API_KEY shared.
func CostCapAllowedForRole(ctx context.Context, tenantID, role string) bool {
	if CapRDB == nil {
		return true
	}
	cap := MoneyCap
	switch role {
	case "client":
		cap = 20
	case "pegawai":
		cap = 80
	case "owner":
		cap = 150
	}
	key := CapKeyForRole(tenantID, role)
	n, err := CapRDB.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		CapRDB.Expire(ctx, key, 24*time.Hour)
	}
	// Also increment legacy global key for backward compat / total view
	CapRDB.Incr(ctx, CapKey(tenantID))
	return n <= int64(cap)
}

// CapKeyForRole is the per-role key: agcap:{tenant}:{role}:{date}
func CapKeyForRole(tenantID, role string) string {
	return "agcap:" + tenantID + ":" + role + ":" + time.Now().Format("2006-01-02")
}

// Per-person RPD: agcap:{tenant}:{role}:{user}:{date}
func RPDKeyForUser(tenantID, role, userID string) string {
	if userID == "" {
		userID = "anon"
	}
	return "rpd:" + tenantID + ":" + role + ":" + userID + ":" + time.Now().Format("2006-01-02")
}

// RPDCapAllowedForUser checks per-person daily cap (client 30, pegawai 80, owner 150)
// Returns false when the person has hit the cap.
func RPDCapAllowedForUser(ctx context.Context, tenantID, role, userID string) bool {
	if CapRDB == nil {
		return true
	}
	cap := 150
	switch role {
	case "client":
		cap = 30
	case "pegawai":
		cap = 80
	case "owner":
		cap = 150
	}
	key := RPDKeyForUser(tenantID, role, userID)
	n, err := CapRDB.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		CapRDB.Expire(ctx, key, 24*time.Hour)
	}
	return n <= int64(cap)
}

// Hardcoded owner contact for abuse fallback
const OwnerContact = "0815-9408-136 (Owner)"
const HardcodedAbuseReply = "Maaf, batas tanya harian habis. Silakan hubungi nomor berikut untuk info lebih lanjut: 0815-9408-136 (Owner)."
const HardcodedScopeReply = "Maaf, saya hanya bisa bantu seputar Centrum Badminton Arena (jadwal, harga, booking, fasilitas). Silakan hubungi nomor berikut untuk info lebih lanjut: 0815-9408-136 (Owner)."

// ---- tools ----

// ---- shared money basis ----
//
// The agent used to have three different answers for "how much money is
// there". query_income and query_net_profit summed
// COALESCE(override_price, price) over bookings — the value COMMITTED —
// while query_range_summary and closing_report summed sports_payments —
// the money actually RECEIVED. The Laporan screen the owner reads is
// payments-based, so the bot and the screen could disagree about the same
// day, and neither tool was wrong about its own definition.
//
// Both numbers matter, and conflating them is worse than picking one. So
// every tool loads them together from this one place and reports the
// basis explicitly: "terima" (received) is the headline, "terpesan"
// (committed) is shown alongside so the gap is always visible.

// basis is one range's money, both ways.
type basis struct {
	Cash   float64 // payments, method=cash
	QRIS   float64 // payments, method=qris
	Other  float64 // payments, any other method (transfer/card/...)
	Booked float64 // COALESCE(override_price, price) over live bookings
	Kantin float64 // canteen sales
	Count  int     // payments counted
}

// Received is money actually taken: payments, plus canteen sales.
func (b basis) Received() float64 { return b.Cash + b.QRIS + b.Other + b.Kantin }

// loadBasis reads both bases for [from, to). Payment and expense dates are
// timestamptz and are compared in WIB, matching how they are entered;
// booking and expense dates are plain dates.
func loadBasis(ctx context.Context, tx pgx.Tx, tenantID, from, to string) (basis, error) {
	var b basis
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount) FILTER (WHERE method='cash'),0),
		        COALESCE(SUM(amount) FILTER (WHERE method='qris'),0),
		        COALESCE(SUM(amount) FILTER (WHERE method NOT IN ('cash','qris')),0),
		        COUNT(*)
		   FROM sports_payments
		  WHERE tenant_id=$1
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
	return b, tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(total),0) FROM sports_kantin_sales
		  WHERE tenant_id=$1
		    AND (created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
		    AND (created_at AT TIME ZONE 'Asia/Jakarta')::date <  $3::date`,
		tenantID, from, to).Scan(&b.Kantin)
}

// describe renders the basis in one sentence so every tool phrases the
// same two numbers the same way.
func (b basis) describe() string {
	return fmt.Sprintf("terima %.0f (cash %.0f, qris %.0f, kantin %.0f), teresan %.0f",
		b.Received(), b.Cash, b.QRIS, b.Kantin, b.Booked)
}

func queryNetProfitTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "query_net_profit",
		Description: "Net profit over a date range, on the money actually received. Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD)}. Defaults to this month. Revenue = payments + kantin, minus expenses. Also reports the committed booking value for comparison.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := defaultMonthRange(p.From, p.To)
			var b basis
			var expenses float64
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				var err error
				if b, err = loadBasis(ctx, tx, tenantID, from, to); err != nil {
					return err
				}
				return tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount),0) FROM sports_expenses
					  WHERE tenant_id=$1 AND spent_at >= $2::date AND spent_at < $3::date`,
					tenantID, from, to).Scan(&expenses)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			// Revenue is the money in, not the money promised. Summing
			// bookings here made a venue look profitable while nothing
			// had been collected.
			net := b.Received() - expenses
			return fmt.Sprintf("net profit %.0f (%s to %s): %s, expenses %.0f",
				net, from, to, b.describe(), expenses)
		},
	}
}

func queryRangeSummaryTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "query_range_summary",
		Description: "Range summary. Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD)}. Revenue cash/qris/kantin, expenses, net profit, payment and booking counts, best revenue day, and the busiest booking hours.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := defaultMonthRange(p.From, p.To)
			var b basis
			var expenses float64
			var bookings int
			var bestDay string
			var bestDayAmt float64
			peak := map[int]int{}
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				var err error
				if b, err = loadBasis(ctx, tx, tenantID, from, to); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount),0) FROM sports_expenses
					  WHERE tenant_id=$1 AND spent_at >= $2::date AND spent_at < $3::date`,
					tenantID, from, to).Scan(&expenses); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings
					  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
					    AND status NOT IN ('cancelled','no_show')`,
					tenantID, from, to).Scan(&bookings); err != nil {
					return err
				}
				// Best day is the day money actually came in, matching the
				// rest of the tool. Ordering by booking value instead
				// would crown a day full of unpaid bookings.
				if err := tx.QueryRow(ctx,
					`SELECT (paid_at AT TIME ZONE 'Asia/Jakarta')::date::text, SUM(amount)
					   FROM sports_payments
					  WHERE tenant_id=$1
					    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
					    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3::date
					  GROUP BY 1 ORDER BY 2 DESC LIMIT 1`,
					tenantID, from, to).Scan(&bestDay, &bestDayAmt); err != nil &&
					!errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				// Peak hours come from when bookings start, which is the
				// question a court owner actually asks.
				rows, err := tx.Query(ctx,
					`SELECT EXTRACT(HOUR FROM start_time)::int, COUNT(*)
					   FROM sports_bookings
					  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
					    AND status NOT IN ('cancelled','no_show')
					  GROUP BY 1 ORDER BY 2 DESC LIMIT 3`,
					tenantID, from, to)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var h, n int
					if err := rows.Scan(&h, &n); err != nil {
						return err
					}
					peak[h] = n
				}
				return rows.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			var peakTxt string
			if len(peak) > 0 {
				hours := make([]string, 0, len(peak))
				for h := 0; h < 24; h++ {
					if n, ok := peak[h]; ok {
						hours = append(hours, fmt.Sprintf("%02d:00 x%d", h, n))
					}
				}
				peakTxt = ", peak " + strings.Join(hours, ", ")
			}
			best := "n/a"
			if bestDay != "" {
				best = fmt.Sprintf("%s (%.0f)", bestDay, bestDayAmt)
			}
			return fmt.Sprintf("range %s to %s: %s, expenses %.0f, net %.0f, payments %d, bookings %d, best day %s%s",
				from, to, b.describe(), expenses, b.Received()-expenses, b.Count, bookings, best, peakTxt)
		},
	}
}

func topCustomersTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "top_customers",
		Description: "Top customers by bookings. Args: {limit? (default 5)}. Returns name, visits.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Limit < 1 {
				p.Limit = 5
			}
			if p.Limit > 20 {
				p.Limit = 20
			}
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				// Name resolution falls back in this order: the directory
				// entry, then the name typed on the booking itself, then a
				// literal. The old query only ever read c.name, so every
				// walk-in whose phone was not in the customer directory
				// came back as "?" — the tool filtered on
				// b.customer_name being present, then ignored it.
				//
				// Grouping is by phone, not name: the comment above used
				// to argue for phone and then group by c.name anyway,
				// which both merged two different people who share a name
				// and split one person who appears under a corrected
				// spelling.
				r, err := tx.Query(ctx,
					`SELECT COALESCE(NULLIF(b.customer_phone,''),
					                 NULLIF(c.name,''),
					                 NULLIF(b.customer_name,''),
					                 '?') AS who,
					        MIN(COALESCE(NULLIF(c.name,''), NULLIF(b.customer_name,''), '?')) AS label,
					        COUNT(*) AS visits
					   FROM sports_bookings b
					   LEFT JOIN sports_customers c
					          ON c.tenant_id = b.tenant_id AND c.phone = b.customer_phone
					  WHERE b.tenant_id=$1 AND b.status IN ('confirmed','checked_in','completed')
					    AND (b.customer_name IS NOT NULL AND b.customer_name <> ''
					         OR c.name IS NOT NULL AND c.name <> '')
					  GROUP BY 1
					  ORDER BY visits DESC
					  LIMIT $2`,
					tenantID, p.Limit)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var who, label string
					var visits int
					if err := r.Scan(&who, &label, &visits); err != nil {
						return err
					}
					// Show the phone as well as the name: two bookings
					// under "?" are otherwise indistinguishable, and the
					// whole point of the tool is telling two apart.
					if who != label {
						rows = append(rows, fmt.Sprintf("%s (%s): %d visits", label, who, visits))
					} else {
						rows = append(rows, fmt.Sprintf("%s: %d visits", label, visits))
					}
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no customers yet"
			}
			return strings.Join(rows, "\n")
		},
	}
}

func closingReportTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "closing_report",
		Description: "Daily closing report (WhatsApp-ready). Args: {date? (YYYY-MM-DD)}. Revenue cash/qris, booking count, occupancy, unpaid list.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = time.Now().Format("2006-01-02")
			}
			var cash, qris float64
			var bookings, unpaid int
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount) FILTER (WHERE method='cash'),0), COALESCE(SUM(amount) FILTER (WHERE method='qris'),0)
				   FROM sports_payments WHERE tenant_id=$1 AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date = $2::date`,
					tenantID, p.Date).Scan(&cash, &qris); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings WHERE tenant_id=$1 AND date=$2::date AND status NOT IN ('cancelled','no_show')`,
					tenantID, p.Date).Scan(&bookings); err != nil {
					return err
				}
				return tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings WHERE tenant_id=$1 AND date=$2::date AND payment_status != 'paid' AND status NOT IN ('cancelled','no_show')`,
					tenantID, p.Date).Scan(&unpaid)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("CLOSING REPORT %s\nRevenue: %.0f (cash %.0f, qris %.0f)\nBookings: %d\nUnpaid: %d",
				p.Date, cash+qris, cash, qris, bookings, unpaid)
		},
	}
}

func queryKantinSummaryTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "query_kantin_summary",
		Description: "Kantin (canteen) summary. Args: {date? (YYYY-MM-DD)}. Omzet, modal, gross profit, top products.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = time.Now().Format("2006-01-02")
			}
			var revenue, cogs float64
			var items int
			var top []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(i.unit_price*i.qty),0), COALESCE(SUM(i.unit_cost*i.qty),0), COALESCE(SUM(i.qty),0)
					   FROM sports_kantin_sale_items i
					   JOIN sports_kantin_sales s ON s.id = i.sale_id
					  WHERE i.tenant_id=$1 AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date = $2::date`,
					tenantID, p.Date).Scan(&revenue, &cogs, &items); err != nil {
					return err
				}
				r, err := tx.Query(ctx,
					`SELECT i.product_name, SUM(i.qty) FROM sports_kantin_sale_items i
					   JOIN sports_kantin_sales s ON s.id = i.sale_id
					  WHERE i.tenant_id=$1 AND s.created_at::date = $2::date
					  GROUP BY i.product_name ORDER BY 2 DESC LIMIT 5`,
					tenantID, p.Date)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name string
					var qty int64
					if err := r.Scan(&name, &qty); err != nil {
						return err
					}
					top = append(top, fmt.Sprintf("%s x%d", name, qty))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("kantin %s: omzet %.0f, modal %.0f, laba %.0f, items %d, top: %s",
				p.Date, revenue, cogs, revenue-cogs, items, strings.Join(top, ", "))
		},
	}
}

// wibNow returns now in WIB (UTC+7, no DST). FixedZone avoids tzdata
// dependency on Windows containers.
func wibNow() time.Time {
	return time.Now().In(wibLoc())
}

// wibLoc is the WIB location for ParseInLocation.
func wibLoc() *time.Location {
	return time.FixedZone("WIB", 7*3600)
}

// defaultMonthRange returns (firstOfMonth, firstOfNextMonth) unless
// both bounds are given.
func defaultMonthRange(from, to string) (string, string) {
	if from != "" && to != "" {
		return from, to
	}
	now := wibNow()
	first := now.Format("2006-01") + "-01"
	next := now.AddDate(0, 1, 0).Format("2006-01-02")
	return first, next
}
