-- 030_reminder_claim.sql - claim markers for the two-driver scheduler.
-- DONE: Vercel/Supabase split / STANDARDS A, E
--
-- WHY: there are now two independent delivery drivers for
-- agent_reminders - the in-process Go ticker (long-running hosts) and
-- the Supabase Edge Function (serverless). Both must never deliver the
-- same row.
--
-- FOR UPDATE SKIP LOCKED alone is not enough across drivers: the send
-- is an HTTP call, so holding a row lock for its duration would mean a
-- long-running transaction. Instead each driver atomically CLAIMS a row
-- by flipping status to 'sending' and stamping claimed_at. Drivers only
-- ever look at status='pending', so a claimed row is invisible to the
-- other driver.
--
-- claimed_at doubles as the optimistic-concurrency token: completion
-- UPDATEs are scoped with `AND status='sending' AND claimed_at = $2`,
-- so a send that outlives the reaper window cannot mark a row the
-- reaper has already handed back.
--
-- Idempotent.

ALTER TABLE agent_reminders
  ADD COLUMN IF NOT EXISTS claimed_at timestamptz;

-- The due-rows query filters (status, remind_at); a partial index on
-- pending rows keeps the claim scan index-only for the common case
-- where most rows are already sent.
CREATE INDEX IF NOT EXISTS agent_reminders_claim_idx
  ON agent_reminders(remind_at)
  WHERE status = 'pending';

-- Reaper: find claims orphaned by a crashed driver.
CREATE INDEX IF NOT EXISTS agent_reminders_stale_idx
  ON agent_reminders(claimed_at)
  WHERE status = 'sending';

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_reminders TO app_runtime, app;
