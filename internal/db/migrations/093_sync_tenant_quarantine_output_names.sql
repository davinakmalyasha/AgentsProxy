-- 093_sync_tenant_quarantine_disambiguation.sql — fix the boot RPC.
--
-- PostgreSQL resolves PL/pgSQL output parameter names before table aliases
-- in some expressions, so the original sync_tenant_quarantine's
-- "tenant_id"/"reason" outputs made ON CONFLICT (tenant_id) and the final
-- projection ambiguous at execution time.
--
-- The directive keeps the public return shape unchanged (old binaries can
-- still call it during a rollback window) and tells PL/pgSQL to prefer the
-- table column when a name is ambiguous. No table or data change.

CREATE OR REPLACE FUNCTION private.sync_tenant_quarantine(p_fenced jsonb)
RETURNS TABLE(tenant_id text, reason text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
#variable_conflict use_column
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
