-- 086_usage_client_scope.sql — an app-pool usage insert belongs to one client.
--
-- 033/063 allowed WITH CHECK (true): any app_runtime SQL could write a usage
-- row for another client (inflate or erase a victim's month). The metering
-- path now sets app.client_id in the insert transaction; the policy binds
-- the row to that client. The admin pool bypasses RLS for reporting.
--
-- app.client_id must be the authenticated client's UUID. An unset GUC
-- yields NULL and the insert fails closed.

DROP POLICY IF EXISTS usage_write_runtime ON public.usage;
CREATE POLICY usage_write_runtime ON public.usage
    FOR INSERT
    TO app_runtime
    WITH CHECK (client_id = current_setting('app.client_id', true)::uuid);

COMMENT ON POLICY usage_write_runtime ON public.usage IS
  'app_runtime may insert only the client named by app.client_id; the admin pool bypasses for reporting.';
