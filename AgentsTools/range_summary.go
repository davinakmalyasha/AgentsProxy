package agentstools

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// RangeSummary is every aggregate the money tools need for one [from, to)
// range, read in a single round trip via private.range_summary (migration
// 056). Before, query_range_summary ran 7 statements inside a transaction
// (9 RTs with BEGIN/COMMIT); query_net_profit 6, query_income 5.
//
// The JSON keys match the function output exactly; BestDay is nil when the
// range has no payments (the old code left bestDay "" and printed "n/a").
type RangeSummary struct {
	Cash     float64 `json:"cash"`
	QRIS     float64 `json:"qris"`
	Other    float64 `json:"other"`
	Booked   float64 `json:"booked"`
	Kantin   float64 `json:"kantin"`
	Expenses float64 `json:"expenses"`
	Count    int     `json:"count"`
	Bookings int     `json:"bookings"`

	BestDay    *string  `json:"best_day"`
	BestDayAmt float64  `json:"best_day_amt"`
	Peak       [][2]int `json:"peak"` // [hour,count] pairs, ascending by hour
}

// Basis projects the received/commit figures so the income and net-profit
// tools keep speaking the exact same numbers (Basis.Describe/Received).
func (s RangeSummary) Basis() Basis {
	return Basis{
		Cash: s.Cash, QRIS: s.QRIS, Other: s.Other,
		Booked: s.Booked, Kantin: s.Kantin, Count: s.Count,
	}
}

// Querier is the QueryRow subset both *pgxpool.Pool and pgx.Tx satisfy, so
// callers choose: pool for a standalone single-RT read, tx to fold the
// lookup into a surrounding transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadRangeSummary reads the whole range summary in one round trip. The
// tools call it on the pool directly with no transaction: the function is
// STABLE read-only, and tenant scoping lives in its p_tenant predicate
// (it runs as money_reader, bypassing RLS by design like month_tokens).
func LoadRangeSummary(ctx context.Context, q Querier, tenantID, from, to string) (RangeSummary, error) {
	var s RangeSummary
	var raw []byte
	if err := q.QueryRow(ctx,
		`SELECT private.range_summary($1, $2::date, $3::date)`,
		tenantID, from, to).Scan(&raw); err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	return s, nil
}
