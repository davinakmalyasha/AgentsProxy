-- 091_sync_tenant_quarantine.sql — one boot RPC for durable fences.
--
-- Boot reconciliation used three admin-pool statements (upsert each fence,
-- delete stale boot/reload rows, read all rows). On Vercel that was also the
-- only reason a cold start opened a DIRECT 5432 pool: production migrations
-- are skipped, and this table is the one write the runtime pool could not do.
--
-- This function does the whole reconcile in one statement and returns the
-- authoritative rows. It is owned by postgres (the admin role for this
-- table), granted EXECUTE to app_runtime, and sets search_path empty.
--
-- Semantics match cmd/server.reconcileQuarantine exactly:
--   * every current fence is upserted;
--   * boot:% and reload:% rows not in the current set are removed;
--   * operator/other-driver rows are preserved and returned.
--
-- Additive: one function, one grant.

CREATE OR REPLACE FUNCTION private.sync_tenant_quarantine(p_fenced jsonb)
RETURNS TABLE(tenant_id text, reason text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_keys text[];
BEGIN
  SELECT COALESCE(pg_catalog.array_agg(e.key), '{}'::text[])
    INTO v_keys
    FROM pg_catalog.jsonb_each_text(COALESCE(NULLIF(p_fenced, 'null'::jsonb), '{}'::jsonb)) AS e(key, value);

  INSERT INTO public.tenant_quarantine (tenant_id, reason)
  SELECT e.key, e.value
    FROM pg_catalog.jsonb_each_text(COALESCE(NULLIF(p_fenced, 'null'::jsonb), '{}'::jsonb)) AS e(key, value)
  ON CONFLICT (tenant_id) DO UPDATE
    SET reason = EXCLUDED.reason,
        quarantined_at = pg_catalog.now();

  DELETE FROM public.tenant_quarantine q
   WHERE (q.reason LIKE 'boot:%' OR q.reason LIKE 'reload:%')
     AND NOT (q.tenant_id = ANY(v_keys));

  RETURN QUERY
  SELECT q.tenant_id, q.reason
    FROM public.tenant_quarantine q
   ORDER BY q.tenant_id;
END;
$function$;

ALTER FUNCTION private.sync_tenant_quarantine(jsonb) OWNER TO postgres;
REVOKE ALL ON FUNCTION private.sync_tenant_quarantine(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.sync_tenant_quarantine(jsonb) TO app_runtime;

COMMENT ON FUNCTION private.sync_tenant_quarantine(jsonb) IS
  'Reconcile boot/reload fences in one call: upsert current, delete stale boot/reload rows, return all durable fences.';
