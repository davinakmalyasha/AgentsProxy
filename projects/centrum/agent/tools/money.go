package centrumbadminton

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/tenantdb"
)

// rangeSummary is every aggregate the money tools need for one [from, to)
// range, read in a single round trip via project_centrum.money_range_summary.
// The function is SECURITY INVOKER; the tenant transaction below provides the
// RLS GUC and resolves the tenant's schema, so a cloned test schema behaves
// exactly like project_centrum.
type rangeSummary struct {
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

// basis projects the received/committed figures so the income and net-profit
// tools keep speaking the exact same numbers.
type basis struct {
	Cash   float64
	QRIS   float64
	Other  float64
	Booked float64
	Kantin float64
	Count  int
}

func (s rangeSummary) basis() basis {
	return basis{
		Cash: s.Cash, QRIS: s.QRIS, Other: s.Other,
		Booked: s.Booked, Kantin: s.Kantin, Count: s.Count,
	}
}

// received is money actually taken: payments plus canteen sales.
func (b basis) received() float64 { return b.Cash + b.QRIS + b.Other + b.Kantin }

// describe renders the basis in one sentence so every tool phrases the same
// two numbers the same way. Idr, not %.0f: one rounding rule everywhere.
func (b basis) describe() string {
	return fmt.Sprintf("terima %s (cash %s, qris %s, kantin %s), teresan %s",
		agentstools.Idr(b.received()), agentstools.Idr(b.Cash),
		agentstools.Idr(b.QRIS), agentstools.Idr(b.Kantin), agentstools.Idr(b.Booked))
}

// loadRangeSummary opens the tenant transaction, calls the project function,
// and decodes its JSON. The function name is project-owned; no platform
// package knows it.
func loadRangeSummary(ctx context.Context, pool *pgxpool.Pool, tenantID, from, to string) (rangeSummary, error) {
	var s rangeSummary
	err := tenantdb.WithTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx,
			`SELECT project_centrum.money_range_summary($1, $2::date, $3::date)`,
			tenantID, from, to).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &s)
	})
	return s, err
}
