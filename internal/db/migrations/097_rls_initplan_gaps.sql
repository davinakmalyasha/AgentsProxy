-- 097_rls_initplan_gaps.sql — close remaining auth_rls_initplan gaps.
--
-- 094 fixed the hottest policies. On a fresh chain the only remaining
-- bare-current_setting policy on a live table is:
--
--   * sports_sessions_isolation (032, public.sports_sessions)
--
-- public.agent_staff_payments was moved to project_centrum and dropped
-- (Centrum 0009); public.telegram_bots was dropped (042). Both are kept
-- here conditionally so an older database that has not run those drops
-- still gets the initplan fix, while a fresh chain (tables gone) stays
-- green. project_centrum.sports_sessions was dropped in Centrum 0005.
--
-- Wrapping in (SELECT ...) makes it an initplan: identical semantics
-- (unset GUC still NULL and denies), one evaluation per statement.
-- Additive and idempotent: no table or data change.

-- sports_sessions (platform session lookup; auth goes through definers,
-- but the policy still guards direct runtime reads)
DROP POLICY IF EXISTS sports_sessions_isolation ON public.sports_sessions;
CREATE POLICY sports_sessions_isolation ON public.sports_sessions
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

-- Older databases only: fix the policy if the pre-drop table is still there.
DO $$
BEGIN
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
