-- 026_telegram_bots_app_grant.sql — T1 fix: runtime grant for the app role.
-- DONE: T1 (grant parity)
--
-- Every migration grants to app_runtime, but the runtime connection in
-- this project logs in as role `app` (Laragon local AND the pooler URL
-- in .env.example). Older tables carry direct `app` grants (applied at
-- setup); new tables must repeat them or the API gets
-- "permission denied" despite correct RLS. Keep this pattern for every
-- new table until the dual-role setup is unified.

GRANT SELECT, INSERT, UPDATE, DELETE ON telegram_bots TO app;
