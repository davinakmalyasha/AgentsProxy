-- 055_drop_dead_reminder_fns.sql — drop the three reminder-driver
-- functions left orphaned by the Telegram removal + Edge Function
-- undeploy (042, main #1).
--
--   private.claim_due_reminders(integer)         (031, redefined 042/048)
--   private.complete_reminder(uuid,timestamptz,text)  (031)
--   private.fail_reminder(uuid,timestamptz,integer)   (031)
--
-- Verified zero callers: the Go ticker claims in raw SQL
-- (internal/reminder), the Edge Function that called them is deleted,
-- and no ops script references them. The quarantine-aware claim body
-- (048) is preserved in history; a future delivery repair re-adds
-- what it needs with CREATE OR REPLACE (see 031/042/048).
--
-- DONE: dead-code pass / STANDARDS A, E

DROP FUNCTION IF EXISTS private.claim_due_reminders(integer);
DROP FUNCTION IF EXISTS private.complete_reminder(uuid, timestamptz, text);
DROP FUNCTION IF EXISTS private.fail_reminder(uuid, timestamptz, integer);
