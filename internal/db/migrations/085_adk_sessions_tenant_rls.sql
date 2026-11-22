-- 085_adk_sessions_tenant_rls.sql — tenant-scope the ADK conversation tables.
--
-- 041 created these tables with USING (true) policies because the session
-- service ran pool-direct. The service now sets app.tenant_id in every
-- transaction; app_name is the tenant slug, so the policy can use it
-- directly without a duplicate tenant_id column or a data rewrite.
--
-- RLS is ENABLE, not FORCE: the owner/admin pool still bypasses it for
-- maintenance, while app_runtime (NOBYPASSRLS) is fenced to the GUC. An
-- unset GUC makes every comparison NULL, so reads return zero rows and
-- writes fail closed.

ALTER TABLE public.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.events ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.app_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.user_states ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sessions_runtime ON public.sessions;
CREATE POLICY sessions_runtime ON public.sessions
    FOR ALL
    TO app_runtime
    USING (app_name = current_setting('app.tenant_id', true))
    WITH CHECK (app_name = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS events_runtime ON public.events;
CREATE POLICY events_runtime ON public.events
    FOR ALL
    TO app_runtime
    USING (app_name = current_setting('app.tenant_id', true))
    WITH CHECK (app_name = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS app_states_runtime ON public.app_states;
CREATE POLICY app_states_runtime ON public.app_states
    FOR ALL
    TO app_runtime
    USING (app_name = current_setting('app.tenant_id', true))
    WITH CHECK (app_name = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS user_states_runtime ON public.user_states;
CREATE POLICY user_states_runtime ON public.user_states
    FOR ALL
    TO app_runtime
    USING (app_name = current_setting('app.tenant_id', true))
    WITH CHECK (app_name = current_setting('app.tenant_id', true));

COMMENT ON POLICY sessions_runtime ON public.sessions IS
  'app_name is the tenant slug. app_runtime must set app.tenant_id; an unset GUC fails closed.';
COMMENT ON POLICY events_runtime ON public.events IS
  'app_name is the tenant slug. app_runtime must set app.tenant_id; an unset GUC fails closed.';
COMMENT ON POLICY app_states_runtime ON public.app_states IS
  'app_name is the tenant slug. app_runtime must set app.tenant_id; an unset GUC fails closed.';
COMMENT ON POLICY user_states_runtime ON public.user_states IS
  'app_name is the tenant slug. app_runtime must set app.tenant_id; an unset GUC fails closed.';
