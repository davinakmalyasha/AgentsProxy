-- 038_series_booking_link.sql — let a booking belong to a subscription.
-- Idempotent.
--
-- WHY: the frontend has always sent recurring_series_id when it generates
-- a subscription's weekly slots, and the backend had nowhere to put it.
-- The column did not exist, the INSERT did not name it, and the row came
-- back without it. Three consequences, all of them silent:
--
--   * "N jadwal ke depan" counted bookings grouped by a field that was
--     always undefined, so it read 0 for every subscription
--   * "+8 minggu" looked for the furthest already-generated instance,
--     found none, and regenerated from start_date -- so every generated
--     week collided with the one already there and the extension added
--     zero while reporting all eight as skipped
--   * there was no way to tell which bookings a subscription owns, so
--     pausing one could not touch them
--
-- ON DELETE SET NULL, not CASCADE: deleting a subscription should not
-- delete the bookings it produced. A customer who booked eight weeks and
-- then had the subscription deleted still turned up for those eight weeks,
-- and the revenue is real. The link is removed, the bookings stay.

ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS recurring_series_id uuid
    REFERENCES sports_recurring_series(id) ON DELETE SET NULL;

-- The hot query is "this subscription's future instances", which always
-- filters on the link plus a date.
CREATE INDEX IF NOT EXISTS sports_bookings_series_idx
  ON sports_bookings(tenant_id, recurring_series_id, date)
  WHERE recurring_series_id IS NOT NULL;