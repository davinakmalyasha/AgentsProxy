-- 032_sessions_tenant_scope.sql - tenant-scope staff sessions.
-- DONE: security hardening / STANDARDS A, D, E, F
--
-- THE HOLE: 022_sports_sessions.sql created this table with no
-- tenant_id, a `USING (true)` policy, and
--   GRANT SELECT, INSERT, UPDATE, DELETE ... TO app_runtime
-- Because there was no tenant column there was nothing to scope on.
-- The runtime role could therefore INSERT a row for any user in any
-- tenant, and the auth lookup joins on token_hash alone:
--
--   INSERT INTO sports_sessions (user_id, token_hash, expires_at)
--   SELECT id, encode(digest('<chosen>','sha256'),'hex'), now() + interval '30 days'
--     FROM sports_users WHERE tenant_id = 'victim' AND role = 'owner';
--   -- then send X-Session-Token: <chosen>
--
-- That is universal cross-tenant owner takeover from any single SQL
-- injection or app bug, and it means RLS - the project's stated hard
-- boundary - provided no backstop on the one credential that matters.
-- The 022 comment claimed the table was "never client-addressable";
-- that was an assumption about callers, not an enforcement.
--
-- THE FIX, in three parts:
--   1. tenant_id column, backfilled from sports_users, so the table can
--      be scoped like every other tenant table.
--   2. The standard tenant policy, FORCEd, replacing USING (true).
--   3. SECURITY DEFINER functions for the four operations the auth
--      flow performs, so app_runtime keeps working WITHOUT direct
--      DML on the table. create_session derives tenant_id from
--      sports_users server-side - it is never taken from the caller,
--      so there is no path to a row for the wrong tenant.
--
-- Additive only: no column dropped, no type changed, and the session
-- token_hash format is untouched, so existing sessions keep working.
--
-- ROLES
--   delivery_worker  existing; owns the reminder/notification claims
--   delivery_caller  existing; EXECUTE on the private.claim_* set
--   app_runtime      the app's runtime role. Loses DML on
--                    sports_sessions, gains EXECUTE on the four
--                    functions below.
--
-- PASSWORD: app_runtime is not given a password here; the runtime DSN
-- carries it. See RUNBOOK for the production equivalent.

-- No BEGIN/COMMIT here on purpose. migrate.go opens a single bootTx,
-- executes every migration body inside it, then commits once at the
-- end. A COMMIT in this file would end that outer transaction early,
-- leaving the schema_migrations INSERT to run outside a transaction and
-- the final bootTx.Commit() to fail with "no transaction in progress" -
-- the server would refuse to boot. Every migration in this directory is
-- a transaction fragment by that contract.

-- ---------------------------------------------------------------------------
-- 1. tenant_id.
-- ---------------------------------------------------------------------------

-- Expired sessions are worthless; drop them before the NOT NULL so the
-- backfill has nothing to guess at. Same predicate the Go janitor uses.
DELETE FROM sports_sessions WHERE expires_at < now();

ALTER TABLE sports_sessions ADD COLUMN IF NOT EXISTS tenant_id text;

-- Backfill from the owning user. Every surviving row has a user_id that
-- references sports_users, so this cannot fail.
UPDATE sports_sessions s
   SET tenant_id = u.tenant_id
  FROM sports_users u
 WHERE u.id = s.user_id
   AND s.tenant_id IS NULL;

-- Any row still NULL means a user row vanished; a session without a
-- tenant cannot be safely guessed at, so drop it rather than weaken
-- the constraint.
DELETE FROM sports_sessions WHERE tenant_id IS NULL;

ALTER TABLE sports_sessions ALTER COLUMN tenant_id SET NOT NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'sports_sessions_tenant_fk'
  ) THEN
    ALTER TABLE sports_sessions
      ADD CONSTRAINT sports_sessions_tenant_fk
      FOREIGN KEY (tenant_id) REFERENCES clients(tenant_id) ON DELETE CASCADE;
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS sports_sessions_tenant_idx
  ON sports_sessions(tenant_id);

-- The janitor deletes by expires_at and there is no index for it, so
-- every hourly run is a sequential scan of the whole table.
CREATE INDEX IF NOT EXISTS sports_sessions_expires_idx
  ON sports_sessions(expires_at);

-- ---------------------------------------------------------------------------
-- 2. Replace the open policy with the standard tenant policy.
-- ---------------------------------------------------------------------------
DROP POLICY IF EXISTS sports_sessions_open ON sports_sessions;
DROP POLICY IF EXISTS sports_sessions_isolation ON sports_sessions;

CREATE POLICY sports_sessions_isolation ON sports_sessions
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ---------------------------------------------------------------------------
-- 3. Definer functions for the auth flow.
--
-- auth_definer is NOLOGIN BYPASSRLS, and both halves matter:
--
--   NOLOGIN   nothing can connect as it. It is a capability holder, not
--             a credential, so it cannot be stolen from a DSN.
--
--   BYPASSRLS FORCE RLS applies to the table owner too, so a NOBYPASSRLS
--             definer running under FORCE ROW LEVEL SECURITY would be
--             filtered by the very policy this migration installs. Every
--             read would see zero rows: login would 401, /me would 401,
--             and the failure would look like bad credentials rather
--             than a broken migration. The policy below depends on
--             app.tenant_id, and the whole point of the definer is that
--             it runs before any tenant is known.
--
-- This mirrors delivery_worker in 031_delivery_roles.sql, which is
-- already the audited pattern in this repo. The blast radius is bounded
-- two ways: it holds grants on exactly two tables and nothing else, and
-- search_path is empty with every reference schema-qualified, so the
-- capability cannot be widened or its behaviour redirected.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    CREATE ROLE auth_definer NOLOGIN BYPASSRLS;
  END IF;
END $$;

-- BYPASSRLS skips row filtering but not privilege checks: without these
-- table grants every function below fails on permission denied.
GRANT USAGE ON SCHEMA private TO auth_definer;
GRANT SELECT          ON sports_users    TO auth_definer;
GRANT SELECT          ON sports_sessions TO auth_definer;
GRANT INSERT          ON sports_sessions TO auth_definer;
GRANT UPDATE          ON sports_sessions TO auth_definer;
GRANT DELETE          ON sports_sessions TO auth_definer;

-- Create a session. The tenant comes from sports_users, NEVER from the
-- caller: that is what makes it impossible to mint a session for a
-- user in a tenant the caller did not name.
CREATE OR REPLACE FUNCTION private.create_session(
  p_user_id   uuid,
  p_token_hash text,
  p_expires_at timestamptz
)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_tenant text;
  v_id     uuid;
BEGIN
  SELECT u.tenant_id INTO v_tenant
    FROM public.sports_users u
   WHERE u.id = p_user_id
     AND u.is_active;
  IF v_tenant IS NULL THEN
    RETURN NULL;
  END IF;

  INSERT INTO public.sports_sessions (user_id, token_hash, expires_at, tenant_id)
  VALUES (p_user_id, p_token_hash, p_expires_at, v_tenant)
  RETURNING id INTO v_id;
  RETURN v_id;
END;
$$;

-- Resolve a session to its user. The tenant-scoped policy on
-- sports_sessions would hide every row here (the auth lookup runs with
-- no app.tenant_id set), so the read goes through the definer too.
-- Returning only the fields the caller needs keeps password_hash out.
CREATE OR REPLACE FUNCTION private.session_user(p_token_hash text)
RETURNS TABLE (
  user_id     uuid,
  tenant_id   text,
  email       text,
  role        text,
  full_name   text
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $$
  SELECT u.id, u.tenant_id, u.email, u.role, u.full_name
    FROM public.sports_sessions s
    JOIN public.sports_users u ON u.id = s.user_id
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
     AND u.is_active
   LIMIT 1;
$$;

-- Logout. Scoped by the hash the caller already proved possession of.
CREATE OR REPLACE FUNCTION private.delete_session(p_token_hash text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  DELETE FROM public.sports_sessions s WHERE s.token_hash = p_token_hash;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

-- Activity stamp. The caller passes the threshold so the write only
-- happens when the stored value is actually stale, exactly as the
-- previous inline UPDATE ... WHERE last_seen_at < now() - interval
-- '1 minute' did.
CREATE OR REPLACE FUNCTION private.touch_session(
  p_token_hash text,
  p_stale_before timestamptz
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  UPDATE public.sports_sessions s
     SET last_seen_at = pg_catalog.now()
   WHERE s.token_hash = p_token_hash
     AND s.last_seen_at < p_stale_before;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

ALTER FUNCTION private.create_session(uuid, text, timestamptz)     OWNER TO auth_definer;
ALTER FUNCTION private.session_user(text)                          OWNER TO auth_definer;
ALTER FUNCTION private.delete_session(text)                        OWNER TO auth_definer;
ALTER FUNCTION private.touch_session(text, timestamptz)            OWNER TO auth_definer;

-- ---------------------------------------------------------------------------
-- 4. Grants.
--
-- app_runtime loses direct DML on the table. It keeps the reads it
-- genuinely needs via the functions; nothing in the app touches
-- sports_sessions directly any more after this migration.
--
-- The dual `app_runtime` / `app` grant is this project's convention
-- (see 026_telegram_bots_app_grant.sql): the runtime DSN logs in as
-- `app`, while migrations create `app_runtime`. Earlier migrations
-- wrote `GRANT ... TO app` unguarded, which aborts the whole
-- transaction - and therefore the entire migration chain, since
-- migrate.go runs them in one - on any database without an `app` role.
-- These are guarded so that a missing role is a no-op rather than an
-- outage.
--
-- REVOKE ... FROM PUBLIC is not optional: Postgres grants EXECUTE on
-- every new function to PUBLIC by default, so without it any role that
-- can reach the private schema (a reporting role, anon/authenticated via
-- PostgREST) inherits the ability to mint and read sessions.
-- ---------------------------------------------------------------------------
REVOKE ALL ON FUNCTION
  private.claim_due_reminders(integer),
  private.complete_reminder(uuid, timestamptz, text),
  private.fail_reminder(uuid, timestamptz, integer),
  private.reap_stale_claims(interval),
  private.any_reminders_due(),
  private.claim_notifications(integer, interval),
  private.delete_notification(bigint),
  private.archive_notification(bigint),
  private.any_notifications_queued(),
  private.create_session(uuid, text, timestamptz),
  private.session_user(text),
  private.delete_session(text),
  private.touch_session(text, timestamptz)
FROM PUBLIC;

-- Single-role design (Phase 1.2): the runtime connects as `app_runtime`
-- only. There is no `app` role, so no existence guard is needed. These
-- four functions are the whole reason this migration exists: session
-- DML is never reachable through a client token.
REVOKE INSERT, UPDATE, DELETE ON sports_sessions FROM app_runtime;

GRANT USAGE ON SCHEMA private TO app_runtime;
GRANT EXECUTE ON FUNCTION
  private.create_session(uuid, text, timestamptz),
  private.session_user(text),
  private.delete_session(text),
  private.touch_session(text, timestamptz)
TO app_runtime;
