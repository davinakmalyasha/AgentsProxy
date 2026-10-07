-- 048_tenant_quarantine.sql — Phase C: quarantine durable in the database
-- so EVERY driver honors it, not just the Go in-process ones.
--
-- The in-memory registry (internal/tenanthealth) is the fast path for
-- HTTP/agent requests. But the Supabase driver (pg_cron + Edge Functions)
-- cannot see process memory: without a table, a fenced tenant's reminders
-- and notifications would still send on serverless prod. This table is the
-- shared source both sides read.
--
-- Writes: Go boot (fence boot-invalid tenants) and the admin reload
-- endpoint (release on success, fence on failure), both on the admin pool.
-- Reads: the claim functions below (SECURITY DEFINER, explicit predicate)
-- and Go boot reconciliation (load existing rows into the registry).
--
-- Additive only (GOVERNANCE §3): one new table, two function bodies
-- redefined with an extra predicate. No existing rows touched.

CREATE TABLE IF NOT EXISTS tenant_quarantine (
  tenant_id      text PRIMARY KEY REFERENCES clients(tenant_id) ON DELETE CASCADE,
  reason         text NOT NULL DEFAULT '',
  quarantined_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tenant_quarantine ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_quarantine FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_quarantine_isolation ON tenant_quarantine;
CREATE POLICY tenant_quarantine_isolation ON tenant_quarantine
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT ON tenant_quarantine TO app_runtime;

-- Claim functions skip fenced tenants. SECURITY DEFINER bypasses RLS, so
-- the predicate is explicit: no row for a quarantined tenant is ever
-- claimed, by any driver.

CREATE OR REPLACE FUNCTION private.claim_notifications(
  p_limit integer DEFAULT 20,
  p_vt    interval DEFAULT interval '60 seconds'
)
RETURNS SETOF jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
  RETURN QUERY
  WITH ready AS (
    SELECT j.id
    FROM public.notify_jobs j
    WHERE j.archived_at IS NULL
      AND j.vt <= pg_catalog.now()
      AND NOT EXISTS (SELECT 1 FROM public.tenant_quarantine q
                       WHERE q.tenant_id = j.tenant_id)
    ORDER BY j.id
    LIMIT GREATEST(p_limit, 0)
    FOR UPDATE SKIP LOCKED
  ), claimed AS (
    UPDATE public.notify_jobs j
       SET read_ct = j.read_ct + 1,
           vt = pg_catalog.now() + p_vt
      FROM ready
     WHERE j.id = ready.id
    RETURNING j.id, j.to_phone, j.text, j.read_ct
  )
  SELECT jsonb_build_object(
    'id',      c.id,
    'to',      c.to_phone,
    'text',    c.text,
    'read_ct', c.read_ct
  )
  FROM claimed c;
END;
$$;

CREATE OR REPLACE FUNCTION private.claim_due_reminders(p_limit integer DEFAULT 20)
RETURNS SETOF jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_ids  uuid[];
  v_ts   timestamptz := pg_catalog.clock_timestamp();
BEGIN
  SELECT coalesce(pg_catalog.array_agg(c.id), '{}'::uuid[])
    INTO v_ids
  FROM (
    SELECT r.id
    FROM public.agent_reminders r
    WHERE r.status = 'pending'
      AND r.remind_at <= pg_catalog.now()
      AND NOT EXISTS (SELECT 1 FROM public.tenant_quarantine q
                       WHERE q.tenant_id = r.tenant_id)
    ORDER BY r.remind_at
    LIMIT GREATEST(p_limit, 0)
    FOR UPDATE SKIP LOCKED
  ) c;

  IF v_ids = '{}'::uuid[] THEN
    RETURN;
  END IF;

  RETURN QUERY
  WITH claimed AS (
    UPDATE public.agent_reminders r
       SET status = 'sending',
           claimed_at = v_ts
      WHERE r.id = ANY(v_ids)
    RETURNING r.id, r.tenant_id, r.chat_id, r.text, r.attempts
  )
  SELECT jsonb_build_object(
    'id',         c.id,
    'tenant_id',  c.tenant_id,
    'chat_id',    c.chat_id,
    'text',       c.text,
    'attempts',   c.attempts,
    'claimed_at', v_ts
  )
  FROM claimed c;
END;
$$;

ALTER FUNCTION private.claim_notifications(integer, interval) OWNER TO delivery_worker;
ALTER FUNCTION private.claim_due_reminders(integer) OWNER TO delivery_worker;
GRANT SELECT ON tenant_quarantine TO delivery_worker;
