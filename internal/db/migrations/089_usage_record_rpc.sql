-- 089_usage_record_rpc.sql — one-round-trip usage metering.
--
-- usage.Record used to open a transaction, set app.client_id, insert, and
-- commit: four round trips before the best-effort quota cache increment.
-- The row is the billing source of truth, so it must not become a queued
-- afterthought, but the transaction framing is not part of that guarantee.
--
-- This definer function performs the same single INSERT in one statement.
-- It is owned by postgres, the only role that may write usage regardless of
-- the FORCE RLS policy; app_runtime gets EXECUTE only. The function cannot
-- read, update, or delete usage, and the FK still rejects an unknown client.
--
-- Additive: one function, one grant. The old transaction path remains valid
-- until the code that calls this is rolled out.

CREATE OR REPLACE FUNCTION private.record_usage(
  p_client     uuid,
  p_tokens_in  integer,
  p_tokens_out integer,
  p_cost       double precision,
  p_model      text,
  p_kind       text
)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = ''
AS $function$
  INSERT INTO public.usage
         (client_id, tokens_in, tokens_out, cost_usd, model, kind)
  VALUES (p_client, p_tokens_in, p_tokens_out,
          p_cost::numeric(10,6), p_model, p_kind);
$function$;

ALTER FUNCTION private.record_usage(uuid, integer, integer, double precision, text, text)
  OWNER TO postgres;
REVOKE ALL ON FUNCTION private.record_usage(uuid, integer, integer, double precision, text, text)
  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.record_usage(uuid, integer, integer, double precision, text, text)
  TO app_runtime;

COMMENT ON FUNCTION private.record_usage(uuid, integer, integer, double precision, text, text) IS
  'Insert one usage row for an already-authenticated client. The caller owns the amount; this function owns the write.';
