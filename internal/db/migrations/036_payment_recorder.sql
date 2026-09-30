-- 036_payment_recorder.sql — record WHO took each payment, so the ledger
-- export can be scoped to a staff member. Idempotent.
--
-- WHY: the export route gates on is_owner/is_staff, and a staff member is
-- meant to see only their own rows. There was no way to do that. The two
-- tables the export reads had:
--
--   sports_bookings.created_by       exists (migration 023)
--   sports_kantin_sales.recorded_by  exists (migration 023)
--   sports_payments.recorded_by      DID NOT EXIST
--
-- so payments could only be attributed to whoever created the BOOKING,
-- which is not the same person: staff routinely take payment for a booking
-- the owner or a colleague entered, and that money would then have shown up
-- in the wrong person's export and nowhere in the actual taker's.
--
-- Backfilled from the booking's creator, which is the best available
-- approximation for historical rows: for those, "who took the money" is
-- unknowable, and the booking's creator is a more honest guess than
-- claiming the tenant-wide total. Rows written from here on record the
-- actual session user.

ALTER TABLE sports_payments
  ADD COLUMN IF NOT EXISTS recorded_by uuid REFERENCES sports_users(id) ON DELETE SET NULL;

-- Historical rows: attribute to the booking's creator.
UPDATE sports_payments p
   SET recorded_by = b.created_by
  FROM sports_bookings b
 WHERE p.booking_id = b.id
   AND p.recorded_by IS NULL
   AND b.created_by IS NOT NULL;

-- Payments not tied to a booking (if any) stay NULL rather than being
-- attributed to an arbitrary user.

CREATE INDEX IF NOT EXISTS sports_payments_recorder_idx
  ON sports_payments(tenant_id, recorded_by, paid_at);

-- Same for the canteen side, which had the column but no index, and whose
-- existing rows were never populated at all.
UPDATE sports_kantin_sales s
   SET recorded_by = b.created_by
  FROM sports_bookings b
 WHERE s.booking_id = b.id
   AND s.recorded_by IS NULL
   AND b.created_by IS NOT NULL;

CREATE INDEX IF NOT EXISTS sports_kantin_sales_recorder_idx
  ON sports_kantin_sales(tenant_id, recorded_by, created_at);
