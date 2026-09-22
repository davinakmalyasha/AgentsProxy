-- 024_reminder_attempts.sql — P2: reminder dead-lettering.
-- DONE: P2 (reminder wiring)
--
-- sendOne used to retry poison rows (bad chat_id, blocked bot) every
-- minute forever without ever marking them. attempts counts send
-- failures; status gains 'dead' for permanently undeliverable rows.

ALTER TABLE agent_reminders
  ADD COLUMN IF NOT EXISTS attempts int NOT NULL DEFAULT 0;

-- status values: pending | sent | cancelled | dead (no CHECK constraint
-- on the column by design; scheduler is the only writer of sent/dead).
