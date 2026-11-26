-- 097_rls_initplan_gaps.sql — close remaining auth_rls_initplan gaps.
--
-- 094 fixed the hottest policies. Remaining bare-current_setting policies
-- live on tables that no longer exist on a fully-migrated database:
--
--   * public.sports_sessions → moved to project_centrum by Centrum 0001,
--     project copy dropped in Centrum 0005 (fully-migrated DBs have neither)
--   * public.agent_staff_payments → moved + dropped (Centrum 0007/0009)
--   * public.telegram_bots → dropped (042)
--
-- Every fix here is conditional on the table existing, so the migration
-- is green on a fresh chain (platform 097 runs before Centrum 0001, table
-- still in public), on prod (tables gone, no-op), and on older databases
-- (tables still present, policy fixed).
--
-- Wrapping in (SELECT ...) makes it an initplan: identical semantics
-- (unset GUC still NULL and denies), one evaluation per statement.
-- Additive and idempotent: no table or data change.

DO $$
BEGIN
  IF to_regclass('public.sports_sessions') IS NOT NULL THEN
    EXECUTE 'DROP POLICY IF EXISTS sports_sessions_isolation ON public.sports_sessions';
    EXECUTE $pol$CREATE POLICY sports_sessions_isolation ON public.sports_sessions
      FOR ALL
      USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
      WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)))$pol$;
  END IF;
  IF to_regclass('project_centrum.sports_sessions') IS NOT NULL THEN
    EXECUTE 'DROP POLICY IF EXISTS sports_sessions_isolation ON project_centrum.sports_sessions';
    EXECUTE $pol$CREATE POLICY sports_sessions_isolation ON project_centrum.sports_sessions
      FOR ALL
      USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
      WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)))$pol$;
  END IF;
  IF to_regclass('public.agent_staff_payments') IS NOT NULL THEN
    EXECUTE 'DROP POLICY IF EXISTS agent_staff_payments_isolation ON public.agent_staff_payments';
    EXECUTE $pol$CREATE POLICY agent_staff_payments_isolation ON public.agent_staff_payments
      FOR ALL
      USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
      WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)))$pol$;
  END IF;
  IF to_regclass('public.telegram_bots') IS NOT NULL THEN
    EXECUTE 'DROP POLICY IF EXISTS telegram_bots_isolation ON public.telegram_bots';
    EXECUTE $pol$CREATE POLICY telegram_bots_isolation ON public.telegram_bots
      FOR ALL
      USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
      WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)))$pol$;
  END IF;
END $$;
