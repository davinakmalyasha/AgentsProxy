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
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/bookings"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/pricing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"time"
)

// errDuplicateKey is the in-tx signal for "idempotency pre-check found the
// original row". It never leaves the tool: the caller maps it to the
// already-booked reply below.
var errDuplicateKey = errors.New("idempotent replay")

func init() { agentstools.Register("centrum", "book_slot", bookSlotTool) }

func bookSlotTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "book_slot", Mutating: true,
		Description: "Book a facility slot. The price is computed from the tenant's " +
			"pricing rule and cannot be set here. Use when the user names a facility, " +
			"date, time and customer. Pass idempotency_key (any unique string per " +
			"booking request) so a retried call returns the original booking " +
			"instead of failing.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"facility_id":{"type":"string","description":"the facility (court) id, from get_availability"},"date":{"type":"string","format":"date"},"start_time":{"type":"string","description":"HH:MM, 24h"},"end_time":{"type":"string","description":"HH:MM, 24h"},"customer_name":{"type":"string"},"customer_phone":{"type":"string"},"deposit":{"type":"number","description":"optional upfront deposit, must be >= 0"},"idempotency_key":{"type":"string","description":"optional unique key per booking request; a repeated key returns the original booking"}},"required":["facility_id","date","start_time","end_time","customer_name"]}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				FacilityID     string  `json:"facility_id"`
				Date           string  `json:"date"`
				StartTime      string  `json:"start_time"`
				EndTime        string  `json:"end_time"`
				CustomerName   string  `json:"customer_name"`
				CustomerPhone  string  `json:"customer_phone"`
				Deposit        float64 `json:"deposit"`
				IdempotencyKey string  `json:"idempotency_key"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "error: invalid args: " + err.Error()
			}
			if strings.TrimSpace(p.CustomerName) == "" {
				return "error: customer_name required"
			}
			if p.Deposit < 0 {
				return "error: deposit must be >= 0"
			}
			idemKey := strings.TrimSpace(p.IdempotencyKey)
			// Resolve facility name (or report which facilities exist).
			var facName, openStr, closeStr string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				// Contract gate (same as the HTTP path).
				if err := records.CheckContractTx(ctx, tx, tenantID, "bookings", bookings.SchemaVersion); err != nil {
					return err
				}
				// Idempotency: a repeated key returns the original row.
				if idemKey != "" {
					var existing string
					qerr := tx.QueryRow(ctx,
						`SELECT id::text FROM sports_bookings WHERE tenant_id=$1 AND idempotency_key=$2`,
						tenantID, idemKey).Scan(&existing)
					if qerr == nil {
						return errDuplicateKey
					}
					if !errors.Is(qerr, pgx.ErrNoRows) {
						return qerr
					}
				}
				err := tx.QueryRow(ctx,
					`SELECT name, open_time::text, close_time::text FROM sports_facilities
					  WHERE id = $1::uuid AND tenant_id = $2 AND status='active' AND deleted_at IS NULL`,
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
						return errors.New("no pricing rule covers that date - ask the owner to set prices first")
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
					return fmt.Errorf("slot is blocked (%s) - pick another time", blockReason)
				}
				// Customer link (same as the HTTP path): a matching phone
				// attaches the directory entry for visit/no-show history.
				var customerID *string
				if strings.TrimSpace(p.CustomerPhone) != "" {
					var cid string
					if err := tx.QueryRow(ctx,
						`SELECT id::text FROM sports_customers WHERE tenant_id=$1 AND phone=$2 AND deleted_at IS NULL`,
						tenantID, strings.TrimSpace(p.CustomerPhone)).Scan(&cid); err == nil {
						customerID = &cid
					}
				}
				var newID string
				if err := tx.QueryRow(ctx,
					`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status, price, deposit, customer_id, source, idempotency_key)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'walk_in',NULLIF($12,'')) RETURNING id::text`,
					tenantID, p.FacilityID, date, start, end, p.CustomerName, p.CustomerPhone, status, price, p.Deposit, customerID, idemKey).Scan(&newID); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				// Idempotent replay: the pre-check found the original.
				if errors.Is(err, errDuplicateKey) {
					return fmt.Sprintf("booked %s on %s %s-%s for %s (already booked)", facName, p.Date, p.StartTime, p.EndTime, p.CustomerName)
				}
				// Lost the insert race with a duplicate key: return the winner.
				if records.IsUniqueViolation(err) && idemKey != "" {
					var existing string
					qerr := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
						return tx.QueryRow(ctx,
							`SELECT id::text FROM sports_bookings WHERE tenant_id=$1 AND idempotency_key=$2`,
							tenantID, idemKey).Scan(&existing)
					})
					if qerr == nil {
						return fmt.Sprintf("booked %s on %s %s-%s for %s (already booked)", facName, p.Date, p.StartTime, p.EndTime, p.CustomerName)
					}
				}
				if errors.Is(err, records.ErrContractMismatch) {
					return "error: schema contract mismatch"
				}
				// Overlap with another active booking - the friendly
				// message the tool Description promises (not raw PG text).
				if strings.Contains(err.Error(), "exclusion") {
					return "error: slot already booked"
				}
				// The block trigger raises the same SQLSTATE as the
				// double-booking exclusion, so it lands here too.
				if strings.Contains(err.Error(), "blocked") {
					return "error: slot is blocked - pick another time"
				}
				return "error: " + err.Error()
			}
			if notify := bookings.NotifyFunc; notify != nil {
				notify(tenantID, p.CustomerPhone, fmt.Sprintf(
					"Booking confirmed: %s at %s, %s %s-%s",
					p.CustomerName, facName, p.Date, p.StartTime, p.EndTime))
			}
			return fmt.Sprintf("booked %s on %s %s-%s for %s", facName, p.Date, p.StartTime, p.EndTime, p.CustomerName)
		},
	}
}
