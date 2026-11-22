-- 0005_drop_legacy_sessions.sql — contract step for project-scoped sessions.
--
-- 0004 switched every auth function to public.staff_sessions. The old
-- project_centrum.sports_sessions table is no longer read or written. Drop
-- it after the contract window so a fresh database and production converge
-- on the same schema.
--
-- Release order: 0004, deploy the binary that uses staff_sessions, then
-- apply this migration.

DROP TABLE IF EXISTS project_centrum.sports_sessions;
