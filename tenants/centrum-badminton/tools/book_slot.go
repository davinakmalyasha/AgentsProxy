// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/pricing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"time"
)

func init() { agentstools.Register("centrum", "book_slot", bookSlotTool) }

func bookSlotTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "book_slot", Mutating: true,
		Description: "Book a facility slot. Args: {facility_id, date (YYYY-MM-DD), start_time (HH:MM), " +
			"end_time (HH:MM), customer_name, customer_phone?}. The price is computed from the tenant's " +
			"pricing rule and cannot be set here. Returns booking id or an error like 'slot already booked'.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				FacilityID    string `json:"facility_id"`
				Date          string `json:"date"`
				StartTime     string `json:"start_time"`
				EndTime       string `json:"end_time"`
				CustomerName  string `json:"customer_name"`
				CustomerPhone string `json:"customer_phone"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "error: invalid args: " + err.Error()
			}
			if strings.TrimSpace(p.CustomerName) == "" {
				return "error: customer_name required"
			}
			// Resolve facility name (or report which facilities exist).
			var facName, openStr, closeStr string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				err := tx.QueryRow(ctx,
					`SELECT name, open_time::text, close_time::text FROM sports_facilities
					  WHERE id = $1::uuid AND tenant_id = $2 AND status='active'`,
					p.FacilityID, tenantID).Scan(&facName, &openStr, &closeStr)
				if err != nil {
					return errors.New("unknown facility")
				}
				// Validate time range quickly. Times may be HH:MM or
				// HH:MM:SS (Postgres and LLMs both); normalize to HH:MM.
				norm := func(s string) string {
					parts := strings.Split(strings.TrimSpace(s), ":")
					if len(parts) >= 2 {
						return parts[0] + ":" + parts[1]
					}
					return s
				}
				start, e1 := time.Parse("15:04", norm(p.StartTime))
				end, e2 := time.Parse("15:04", norm(p.EndTime))
				if e1 != nil || e2 != nil {
					return errors.New("start_time/end_time must be HH:MM")
				}
				if !end.After(start) {
					return errors.New("end_time must be after start_time")
				}
				open, e3 := time.Parse("15:04", norm(openStr))
				closeT, e4 := time.Parse("15:04", norm(closeStr))
				if e3 != nil || e4 != nil {
					return errors.New("facility hours misconfigured")
				}
				if start.Before(open) || end.After(closeT) {
					return fmt.Errorf("outside facility hours %s-%s", openStr, closeStr)
				}
				date, err := time.Parse("2006-01-02", p.Date)
				if err != nil {
					return errors.New("date must be YYYY-MM-DD")
				}
				status := "confirmed"
				// Price comes from the tenant's pricing rule, never from
				// the model.
				//
				// This used to INSERT the model's `price` argument
				// verbatim. When the model omitted it -- which it often
				// did, since the argument is optional in the schema -- the
				// booking committed at Rp0. And an Rp0 booking can never
				// be paid: the recalc trigger treats a 0 price as unpaid
				// forever, and the payments handler then computes
				// outstanding = 0 - 0 = 0 and refuses every payment with
				// "already paid". One bad tool call produced a booking that
				// was permanently uncollectable.
				//
				// So compute it exactly as the HTTP create path does, and
				// enforce the same opening window.
				price, err := pricing.PriceFor(ctx, tx, tenantID, date, start, end)
				if err != nil {
					if errors.Is(err, pricing.ErrNoRule) {
						return errors.New("no pricing rule covers that date Ã¢â‚¬â€ ask the owner to set prices first")
					}
					return err
				}
				rule, err := pricing.RuleFor(ctx, tx, tenantID, date)
				if err != nil {
					return err
				}
				ruleOpen, e1 := time.Parse("15:04", norm(rule.OpenTime))
				ruleClose, e2 := time.Parse("15:04", norm(rule.CloseTime))
				if e1 != nil || e2 != nil {
					return errors.New("pricing rule hours misconfigured")
				}
				if start.Before(ruleOpen) || end.After(ruleClose) {
					return fmt.Errorf("outside opening hours %s-%s", rule.OpenTime, rule.CloseTime)
				}
				// A blocked court is not bookable. Checked here because
				// this tool INSERTs directly and so never reaches the
				// bookings handler's own check; the trigger in migration
				// 037 is the backstop for anything that slips past both.
				var blockReason string
				err = tx.QueryRow(ctx,
					`SELECT reason FROM sports_court_blocks
					  WHERE tenant_id=$1 AND block_date=$2::date
					    AND (facility_id=$3::uuid OR facility_id IS NULL)
					    AND start_time < $4::time AND end_time > $5::time
					  LIMIT 1`,
					tenantID, date, p.FacilityID, end, start).Scan(&blockReason)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if blockReason != "" {
					return fmt.Errorf("slot is blocked (%s) Ã¢â‚¬â€ pick another time", blockReason)
				}
				_, err = tx.Exec(ctx,
					`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status, price)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
					tenantID, p.FacilityID, date, start, end, p.CustomerName, p.CustomerPhone, status, price)
				if err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				// Overlap with another active booking Ã¢â€ â€™ the friendly
				// message the tool Description promises (not raw PG text).
				if strings.Contains(err.Error(), "exclusion") {
					return "error: slot already booked"
				}
				// The block trigger raises the same SQLSTATE as the
				// double-booking exclusion, so it lands here too.
				if strings.Contains(err.Error(), "blocked") {
					return "error: slot is blocked Ã¢â‚¬â€ pick another time"
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("booked %s on %s %s-%s for %s", facName, p.Date, p.StartTime, p.EndTime, p.CustomerName)
		},
	}
}
