-- 037_court_blocks_guard.sql — make a court block actually block.
-- Idempotent.
--
-- WHY: a "blockir" was cosmetic. The availability board and the schedule
-- grid grey the slot out, but nothing enforced it on the write path:
--
--   * POST /v1/sports/bookings/ never read sports_court_blocks
--   * the agent's book_slot does a raw INSERT into sports_bookings,
--     bypassing the handler entirely
--   * /app/booking/baru?date&court&hour is reachable by URL
--
-- So a court could be blocked for maintenance and still be booked, by
-- anyone, with no error.
--
-- WHY A TRIGGER RATHER THAN A HANDLER CHECK: the existing
-- double-booking guard is a database EXCLUDE constraint, not application
-- code, precisely so no call path can forget it. A block is the same kind
-- of invariant -- an invariant about the row, not about the caller. Putting
-- it in the database means the agent's direct INSERT is covered without a
-- second implementation to keep in step.
--
-- The Go handlers still check first, so the caller gets a readable message
-- instead of a raw Postgres error. This trigger is the backstop.

CREATE OR REPLACE FUNCTION sports_blocked_booking_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
AS $$
DECLARE
  v_reason text;
BEGIN
  -- Half-open overlap: a booking may start exactly when a block ends.
  -- A block of 09:00-12:00 must not refuse a 12:00-13:00 booking.
  SELECT b.reason INTO v_reason
    FROM sports_court_blocks b
   WHERE b.tenant_id = NEW.tenant_id
     AND b.block_date = NEW.date
     AND (b.facility_id = NEW.facility_id OR b.facility_id IS NULL)
     AND b.start_time < NEW.end_time
     AND b.end_time   > NEW.start_time
   LIMIT 1;

  IF v_reason IS NOT NULL THEN
    RAISE EXCEPTION
      'slot is blocked (%): court % on % %-%',
      v_reason, NEW.facility_id, NEW.date,
      to_char(NEW.start_time, 'HH24:MI'), to_char(NEW.end_time, 'HH24:MI')
      USING ERRCODE = 'exclusion_violation';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS sports_bookings_blocked ON sports_bookings;
CREATE TRIGGER sports_bookings_blocked
  BEFORE INSERT OR UPDATE OF date, start_time, end_time, facility_id
  ON sports_bookings
  FOR EACH ROW
  EXECUTE FUNCTION sports_blocked_booking_guard();

-- Reads that reason from the message, so it has to be findable. Kept as a
-- stable prefix so callers can match on it without depending on the whole
-- sentence, which is free to gain detail later.
COMMENT ON FUNCTION sports_blocked_booking_guard() IS
  'Rejects bookings that overlap a court block. Matches ERRCODE 23P01 like the double-booking exclusion so existing 409 handling covers both.';