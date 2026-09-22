// Phase 7: money-reporting tools ported from CentrumBadminton's Gemini
// assistant (net profit, range summary, top customers, closing report,
// kantin summary) + a per-tenant daily cost cap.
//
// DONE: Phase 7 (agent money tools) / STANDARDS A, E
package agent

import (
	"context"
	"encoding/json"
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

func queryNetProfitTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "query_net_profit",
		Description: "Net profit over a date range. Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD)}. Defaults to this month. Revenue (confirmed+paid bookings + kantin) minus expenses.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := defaultMonthRange(p.From, p.To)
			var revenue, expenses float64
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				// Booking revenue: price of confirmed+checked_in+completed.
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(COALESCE(override_price, price)),0) FROM sports_bookings
					  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
					    AND status IN ('confirmed','checked_in','completed')`,
					tenantID, from, to).Scan(&revenue); err != nil {
					return err
				}
				// Kantin revenue.
				var kantin float64
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(total),0) FROM sports_kantin_sales
				  WHERE tenant_id=$1 AND (created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date AND (created_at AT TIME ZONE 'Asia/Jakarta')::date < $3::date`,
					tenantID, from, to).Scan(&kantin); err != nil {
					return err
				}
				revenue += kantin
				return tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount),0) FROM sports_expenses
					  WHERE tenant_id=$1 AND spent_at >= $2::date AND spent_at < $3::date`,
					tenantID, from, to).Scan(&expenses)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("net profit %.0f (%s to %s): revenue %.0f, expenses %.0f", revenue-expenses, from, to, revenue, expenses)
		},
	}
}

func queryRangeSummaryTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "query_range_summary",
		Description: "Range summary. Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD)}. Revenue cash/qris, expenses, net profit, booking count, best day, peak hours.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := defaultMonthRange(p.From, p.To)
			var cash, qris, expenses float64
			var bookings int
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount) FILTER (WHERE method='cash'),0), COALESCE(SUM(amount) FILTER (WHERE method='qris'),0)
				   FROM sports_payments WHERE tenant_id=$1 AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date < $3::date`,
					tenantID, from, to).Scan(&cash, &qris); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(amount),0) FROM sports_expenses
					  WHERE tenant_id=$1 AND spent_at >= $2::date AND spent_at < $3::date`,
					tenantID, from, to).Scan(&expenses); err != nil {
					return err
				}
				return tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings
					  WHERE tenant_id=$1 AND date >= $2::date AND date < $3::date
					    AND status NOT IN ('cancelled','no_show')`,
					tenantID, from, to).Scan(&bookings)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("range %s to %s: revenue cash %.0f, qris %.0f, total %.0f, expenses %.0f, net %.0f, bookings %d",
				from, to, cash, qris, cash+qris, expenses, cash+qris-expenses, bookings)
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
				// Aggregate visits in one grouped query. The customer
				// identity is phone (the unique key) — name alone
				// miscounts on renames/duplicates — so group by name.
				r, err := tx.Query(ctx,
					`SELECT COALESCE(NULLIF(c.name, ''), '?'), COUNT(*) AS visits
					   FROM sports_bookings b
					   LEFT JOIN sports_customers c ON c.tenant_id = b.tenant_id AND c.phone = b.customer_phone
					  WHERE b.tenant_id=$1 AND b.status IN ('confirmed','checked_in','completed')
					    AND b.customer_name IS NOT NULL AND b.customer_name <> ''
					  GROUP BY c.name
					  ORDER BY visits DESC
					  LIMIT $2`,
					tenantID, p.Limit)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name string
					var visits int
					if err := r.Scan(&name, &visits); err != nil {
						return err
					}
					rows = append(rows, fmt.Sprintf("%s: %d visits", name, visits))
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
