-- 0011_contract_range_summary.sql — retire the platform money definer.
--
-- 0008 moved the aggregate to project_centrum.money_range_summary as a
-- SECURITY INVOKER function. The old private.range_summary and its
-- money_reader role are now unused: no function, view, policy, default ACL,
-- or foreign key depends on them.
--
-- This is a project migration because Centrum 0004 and 0006 recreate the old
-- function and its grants after the platform chain runs. The cleanup must run
-- after those migrations on a fresh database too.
--
-- Contract step: destructive by design. Apply only after the stability window.

DROP FUNCTION IF EXISTS private.range_summary(text, date, date);

DO $$
DECLARE
  v_role  oid;
  v_owned text;
BEGIN
  SELECT oid INTO v_role FROM pg_roles WHERE rolname = 'money_reader';
  IF v_role IS NULL THEN
    RETURN;
  END IF;

  -- DROP OWNED BY would silently drop anything unexpected the role owns.
  -- Refuse instead, so a future grant cannot turn this cleanup into data loss.
  SELECT string_agg(kind || ' ' || name, ', ')
    INTO v_owned
    FROM (
      SELECT 'function' AS kind, p.oid::regprocedure::text AS name
        FROM pg_proc p WHERE p.proowner = v_role
      UNION ALL
      SELECT 'relation', c.oid::regclass::text
        FROM pg_class c WHERE c.relowner = v_role
      UNION ALL
      SELECT 'schema', n.oid::regnamespace::text
        FROM pg_namespace n WHERE n.nspowner = v_role
      UNION ALL
      SELECT 'type', t.oid::regtype::text
        FROM pg_type t WHERE t.typowner = v_role
    ) q;

  IF v_owned IS NOT NULL THEN
    RAISE EXCEPTION 'money_reader still owns unexpected objects: %', v_owned;
  END IF;

  EXECUTE 'DROP OWNED BY money_reader';
  EXECUTE 'DROP ROLE money_reader';
END $$;

COMMENT ON FUNCTION project_centrum.money_range_summary(text, date, date) IS
  'Centrum money aggregate. SECURITY INVOKER: call inside tenantdb.WithTx so RLS and the tenant schema apply. Replaces private.range_summary.';
