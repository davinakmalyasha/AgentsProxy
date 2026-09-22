-- 027_retention_cleanup.sql — bounded-row janitor queries for tables
-- that otherwise grow forever. Idempotent. The Go janitor calls these
-- on a timer with LIMITs; the DELETE predicates use the existing
-- indexes (sports_sessions_token_idx, agent_reminders_due_idx).
-- DONE: retention hardening / STANDARDS A, E

-- Expired sessions: 30-day rows pile up per login; drop rows whose
-- token expired more than 7 days ago (keeps a short audit tail).
DELETE FROM sports_sessions
 WHERE expires_at < now() - interval '7 days';

-- Sent reminders are immutable history; keep 90 days.
DELETE FROM agent_reminders
 WHERE status = 'sent'
   AND created_at < now() - interval '90 days';

-- Dead-lettered reminders are diagnostics; keep 30 days.
DELETE FROM agent_reminders
 WHERE status = 'dead'
   AND created_at < now() - interval '30 days';
