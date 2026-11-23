-- 087_admin_audit_log.sql — append-only audit of the platform admin surface.
--
-- The /v1/admin API can create tenants, mint their first owner, change
-- quotas/features, and edit the CORS registry. Those are the highest-impact
-- operations in the product, and until now they left only a request log line.
-- This table records every admin request before the handler runs, then fills
-- in the response status and duration.
--
-- Actor is a short fingerprint of the configured ADMIN_API_KEY, never the key
-- itself. The admin surface currently has one key; the fingerprint still makes
-- a key rotation visible and distinguishes historical rows after a rotation.
--
-- No request body is stored: POST /clients/{id}/owner carries a password, and
-- an audit trail must not become a secondary credential store. Query
-- parameters and the route pattern are enough to reconstruct the action.
--
-- Owner-only by construction: the admin pool owns/writes the table; the
-- runtime role gets no grant. RLS is enabled without a policy so a future
-- accidental grant still returns zero rows. FORCE is deliberately not set:
-- the owner (admin pool) must be able to write and read.
--
-- Additive: one table and indexes. No existing object is changed.

CREATE TABLE IF NOT EXISTS public.admin_audit_log (
  id          bigserial PRIMARY KEY,
  occurred_at timestamptz NOT NULL DEFAULT now(),
  actor       text NOT NULL,
  action      text NOT NULL,
  target      text NOT NULL DEFAULT '',
  method      text NOT NULL,
  path        text NOT NULL,
  status      integer NOT NULL DEFAULT 0,
  duration_ms integer NOT NULL DEFAULT 0,
  request_id  text NOT NULL DEFAULT '',
  remote_addr text NOT NULL DEFAULT '',
  detail      jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS admin_audit_log_occurred_idx
  ON public.admin_audit_log (occurred_at DESC);
CREATE INDEX IF NOT EXISTS admin_audit_log_actor_idx
  ON public.admin_audit_log (actor, occurred_at DESC);
CREATE INDEX IF NOT EXISTS admin_audit_log_action_idx
  ON public.admin_audit_log (action, occurred_at DESC);

ALTER TABLE public.admin_audit_log ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON public.admin_audit_log FROM PUBLIC;
DO $$
BEGIN
  IF to_regclass('public.admin_audit_log_id_seq') IS NOT NULL THEN
    REVOKE ALL ON SEQUENCE public.admin_audit_log_id_seq FROM PUBLIC;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    REVOKE ALL ON public.admin_audit_log FROM app_runtime;
    IF to_regclass('public.admin_audit_log_id_seq') IS NOT NULL THEN
      REVOKE ALL ON SEQUENCE public.admin_audit_log_id_seq FROM app_runtime;
    END IF;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL ON public.admin_audit_log FROM anon;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL ON public.admin_audit_log FROM authenticated;
  END IF;
END $$;

COMMENT ON TABLE public.admin_audit_log IS
  'Append-only audit of /v1/admin requests. Actor is an ADMIN_API_KEY fingerprint; bodies are never stored.';
