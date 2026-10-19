-- 066_mint_report_upsert.sql — repeat download links for one object.
--
-- 053's mint_report only ignored duplicate token hashes. report_objects
-- has UNIQUE(object_key), so minting a SECOND time for the same file —
-- which the deferred job link does on every click — raised 23505. Worse,
-- MintReportToken's failure path then deleted the shared object, so a
-- retry click destroyed the file earlier links pointed at.
--
-- One object = one manifest row; re-minting ROTATES that row's token
-- hash and expiry (the sweeper deletes by hash, so it follows the
-- rotation). Old Redis tickets stay valid until their own TTL.
--
-- Additive-only: function replace + re-asserted owner/grants.

CREATE OR REPLACE FUNCTION private.mint_report(
  p_hash text, p_tenant text, p_object_key text,
  p_size integer, p_ttl_secs integer)
RETURNS boolean
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = '' AS $$
  INSERT INTO public.report_objects
         (token_hash, tenant_id, object_key, size_bytes, expires_at)
  VALUES (p_hash, p_tenant, p_object_key, p_size,
          pg_catalog.now() + make_interval(secs => p_ttl_secs))
  ON CONFLICT (object_key) DO UPDATE
     SET token_hash = EXCLUDED.token_hash,
         size_bytes = EXCLUDED.size_bytes,
         expires_at = EXCLUDED.expires_at;
  SELECT true;
$$;
ALTER FUNCTION private.mint_report(text, text, text, integer, integer) OWNER TO report_definer;
-- The upsert rotates the manifest row: report_definer needs UPDATE
-- (053 granted only SELECT/INSERT/DELETE).
GRANT UPDATE ON public.report_objects TO report_definer;
REVOKE ALL ON FUNCTION private.mint_report(text, text, text, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.mint_report(text, text, text, integer, integer) TO app_runtime;
