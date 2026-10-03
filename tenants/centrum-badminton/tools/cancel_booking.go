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
)

func init() { agentstools.Register("centrum", "cancel_booking", cancelBookingTool) }

func cancelBookingTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "cancel_booking", Mutating: true,
		Description: "Cancel a booking by id. Args: {booking_id}. Returns cancelled.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				BookingID string `json:"booking_id"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.BookingID == "" {
				return "error: booking_id required"
			}
			// Cancellation is only meaningful for upcoming/active
			// bookings Ã¢â‚¬â€ completed history must stay immutable.
			var status string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT status FROM sports_bookings
					  WHERE id=$1::uuid AND tenant_id=$2`,
					p.BookingID, tenantID).Scan(&status)
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: no booking found with that id"
				}
				return "error: " + err.Error()
			}
			if status == "cancelled" {
				return "error: booking already cancelled"
			}
			if status == "completed" || status == "no_show" {
				return "error: booking already " + status + " Ã¢â‚¬â€ cannot cancel"
			}
			err = records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`UPDATE sports_bookings SET status='cancelled', updated_at=now()
					  WHERE id=$1::uuid AND tenant_id=$2 AND status != 'cancelled'`,
					p.BookingID, tenantID)
				return err
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return "booking cancelled"
		},
	}
}
