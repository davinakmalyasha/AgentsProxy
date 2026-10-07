// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"errors"
	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
)

func init() { agentstools.Register("centrum", "cancel_booking", cancelBookingTool) }

func cancelBookingTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "cancel_booking", Mutating: true,
		Description: "Cancel a booking by id. Use only when the user asks to cancel; " +
			"never volunteer it. Find the id with list_bookings first. " +
			"Payment is offline: cancelling never refunds automatically. If the booking " +
			"is paid or partially paid, a note describing the offline cash handling is " +
			"required and is stored on the booking.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"booking_id":{"type":"string","description":"the booking id"},"note":{"type":"string","description":"required when the booking is paid or partially paid; how offline cash was handled","maxLength":500}},"required":["booking_id"]}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				BookingID string `json:"booking_id"`
				Note      string `json:"note"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.BookingID == "" {
				return "error: booking_id required"
			}
			note := strings.TrimSpace(p.Note)
			if len(note) > 500 {
				return "error: note too long (max 500 chars)"
			}
			// Cancellation is only meaningful for upcoming/active
			// bookings — completed history must stay immutable.
			// Single transaction with FOR UPDATE: the state read and the
			// state write cannot interleave with another canceller, and a
			// paid booking cannot slip through without its offline note.
			var status, paymentStatus string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if err := tx.QueryRow(ctx,
					`SELECT status, payment_status FROM sports_bookings
					  WHERE id=$1::uuid AND tenant_id=$2 FOR UPDATE`,
					p.BookingID, tenantID).Scan(&status, &paymentStatus); err != nil {
					return err
				}
				if status == "cancelled" {
					return errors.New("booking already cancelled")
				}
				if status == "completed" || status == "no_show" {
					return errors.New("booking already " + status + " — cannot cancel")
				}
				if paymentStatus != "unpaid" && note == "" {
					return errors.New("paid booking requires a cancellation note describing the offline cash handling")
				}
				_, err := tx.Exec(ctx,
					`UPDATE sports_bookings
					    SET status='cancelled',
					        notes = CASE WHEN $3 = '' THEN notes WHEN notes = '' THEN $3 ELSE notes || E'\n' || $3 END,
					        updated_at=now()
					  WHERE id=$1::uuid AND tenant_id=$2 AND status != 'cancelled'`,
					p.BookingID, tenantID, note)
				return err
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: no booking found with that id"
				}
				return "error: " + err.Error()
			}
			if note == "" {
				return "booking cancelled"
			}
			return "booking cancelled (note recorded)"
		},
	}
}
