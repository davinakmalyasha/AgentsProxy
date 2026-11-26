-- 094_rls_initplan.sql — evaluate the tenant GUC once per query, not per row.
--
-- Supabase flags every policy below for re-evaluating
-- current_setting() per row (auth_rls_initplan). Wrapping the call in a
-- scalar subquery turns it into an initplan evaluated once per statement:
-- identical semantics (unset GUC still yields NULL and denies), one
-- evaluation instead of one per scanned row.
--
-- Semantics are unchanged: same tables, policy names, commands, roles, and
-- predicates — only the evaluation shape changes. DROP + CREATE is used
-- because CREATE OR REPLACE cannot change a policy body.
--
-- Additive and idempotent: DROP IF EXISTS + CREATE, no table or data change.

-- agent_reminders
DROP POLICY IF EXISTS agent_reminders_isolation ON public.agent_reminders;
CREATE POLICY agent_reminders_isolation ON public.agent_reminders
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- app_states (app_name carries the tenant for the ADK session store)
DROP POLICY IF EXISTS app_states_runtime ON public.app_states;
CREATE POLICY app_states_runtime ON public.app_states
  FOR ALL
  TO app_runtime
  USING (app_name = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (app_name = (SELECT current_setting('app.tenant_id', true)));

-- events
DROP POLICY IF EXISTS events_runtime ON public.events;
CREATE POLICY events_runtime ON public.events
  FOR ALL
  TO app_runtime
  USING (app_name = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (app_name = (SELECT current_setting('app.tenant_id', true)));

-- frontend_origins (tenant-scoped writes; the global SELECT stays as-is)
DROP POLICY IF EXISTS frontend_origins_tenant_insert ON public.frontend_origins;
CREATE POLICY frontend_origins_tenant_insert ON public.frontend_origins
  FOR INSERT
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));
DROP POLICY IF EXISTS frontend_origins_tenant_update ON public.frontend_origins;
CREATE POLICY frontend_origins_tenant_update ON public.frontend_origins
  FOR UPDATE
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));
DROP POLICY IF EXISTS frontend_origins_tenant_delete ON public.frontend_origins;
CREATE POLICY frontend_origins_tenant_delete ON public.frontend_origins
  FOR DELETE
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- notify_jobs
DROP POLICY IF EXISTS notify_jobs_isolation ON public.notify_jobs;
CREATE POLICY notify_jobs_isolation ON public.notify_jobs
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- push_subscriptions
DROP POLICY IF EXISTS push_subscriptions_isolation ON public.push_subscriptions;
CREATE POLICY push_subscriptions_isolation ON public.push_subscriptions
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- report_jobs
DROP POLICY IF EXISTS report_jobs_isolation ON public.report_jobs;
CREATE POLICY report_jobs_isolation ON public.report_jobs
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- report_objects
DROP POLICY IF EXISTS report_objects_isolation ON public.report_objects;
CREATE POLICY report_objects_isolation ON public.report_objects
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- sessions
DROP POLICY IF EXISTS sessions_runtime ON public.sessions;
CREATE POLICY sessions_runtime ON public.sessions
  FOR ALL
  TO app_runtime
  USING (app_name = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (app_name = (SELECT current_setting('app.tenant_id', true)));

-- tenant_quarantine
DROP POLICY IF EXISTS tenant_quarantine_isolation ON public.tenant_quarantine;
CREATE POLICY tenant_quarantine_isolation ON public.tenant_quarantine
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- tenant_records (platform surface)
DROP POLICY IF EXISTS tenant_isolation ON public.tenant_records;
CREATE POLICY tenant_isolation ON public.tenant_records
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- tenant_schema_contracts
DROP POLICY IF EXISTS tenant_schema_contracts_isolation ON public.tenant_schema_contracts;
CREATE POLICY tenant_schema_contracts_isolation ON public.tenant_schema_contracts
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- usage (client-scoped insert; the definer RPC is the write path)
DROP POLICY IF EXISTS usage_write_runtime ON public.usage;
CREATE POLICY usage_write_runtime ON public.usage
  FOR INSERT
  TO app_runtime
  WITH CHECK (client_id = (SELECT current_setting('app.client_id', true))::uuid);

-- user_states
DROP POLICY IF EXISTS user_states_runtime ON public.user_states;
CREATE POLICY user_states_runtime ON public.user_states
  FOR ALL
  TO app_runtime
  USING (app_name = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (app_name = (SELECT current_setting('app.tenant_id', true)));
