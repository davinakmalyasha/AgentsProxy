-- 053_perf_indexes_guards_definers.sql — performance + guardrail batch.
--
-- SCOPE (all additive-safe, all IF NOT EXISTS / OR REPLACE / IF EXISTS):
--   §1  Hot-path indexes (agent turns, CRUD lists, availability).
--   §2  Money indexes (partial WIB-day + recorder + trigger support).
--   §3  Tombstone/sort partials on user-visible lists.
--   §4  Queue/reminder/usage indexes + BRIN on append-only ledgers.
--   §5  Drop superseded/duplicate indexes (write amplification).
--   §6  Report definer (fixes report_objects FORCE RLS vs runtime pool).
--   §7  Quota definer (replaces the blanket usage SELECT policy).
--   §8  Booking-guard advisory lock (deadlock → short wait).
--   §9  Payment-status no-op-write guards (MVCC/WAL on the hottest table).
--
-- Plain CREATE INDEX (not CONCURRENTLY): the boot runner applies
-- migrations inside one transaction (see 032), and CONCURRENTLY cannot
-- run in a transaction block. Tables are small; each lock is brief.
-- Verified index-for-index against the Go query shapes before commit.
--
-- DONE: perf pass / STANDARDS A, E

-- ---------------------------------------------------------------------------
-- §1. Hot-path indexes.
-- ---------------------------------------------------------------------------

-- THE agent-turn query (adkrunner pgx_sessions Get): every turn filters
-- (app_name, user_id, session_id) and sorts by ("timestamp" DESC, id).
-- The only existing index is events_pkey (id, app_name, user_id,
-- session_id) — id leads, so the filter cannot use it. Verified
-- production plan: Seq Scan + Sort on every turn.
CREATE INDEX IF NOT EXISTS events_session_time_idx
  ON public.events (app_name, user_id, session_id, "timestamp" DESC, id DESC);

-- Generic CRUD list (records handlers + feature adapter):
-- WHERE vertical + status ORDER BY created_at DESC, id DESC LIMIT n.
-- The existing (tenant_id, vertical, kind) prefix cannot deliver the
-- sort or the cursor bound; every page sorted every matching row.
CREATE INDEX IF NOT EXISTS tenant_records_list_idx
  ON public.tenant_records (tenant_id, vertical, status, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS tenant_records_kind_list_idx
  ON public.tenant_records (tenant_id, vertical, kind, status, created_at DESC, id DESC);

-- Bookings list sorts (date, start_time) but only (tenant_id, date)
-- is indexed; plus a live partial so cancelled/no_show rows leave the
-- scan (availability, closing_report, query_attendance all exclude them).
CREATE INDEX IF NOT EXISTS sports_bookings_tenant_date_time_idx
  ON public.sports_bookings (tenant_id, date DESC, start_time DESC)
  INCLUDE (facility_id, status, price, payment_status);
CREATE INDEX IF NOT EXISTS sports_bookings_live_date_idx
  ON public.sports_bookings (tenant_id, date DESC, start_time DESC)
  WHERE status NOT IN ('cancelled', 'no_show');

-- Customer visit counts join bookings on name/phone (legacy compat for
-- pre-023 rows; 023's customer_id FK is the real fix in application
-- code). Without these the planner hash-joins the tenant's whole
-- booking history per customer-list request.
CREATE INDEX IF NOT EXISTS sports_bookings_name_idx
  ON public.sports_bookings (tenant_id, customer_name);
CREATE INDEX IF NOT EXISTS sports_bookings_phone_idx
  ON public.sports_bookings (tenant_id, customer_phone)
  WHERE customer_phone <> '';

-- ---------------------------------------------------------------------------
-- §2. Money indexes: every money query filters a tombstone column AND a
-- WIB-day expression, and no index carries both — so one predicate is
-- always a heap recheck. These carry both.
-- ---------------------------------------------------------------------------

CREATE INDEX IF NOT EXISTS sports_kantin_sales_live_wibday_idx
  ON public.sports_kantin_sales
     (tenant_id, ((created_at AT TIME ZONE 'Asia/Jakarta')::date) DESC)
  WHERE voided_at IS NULL;
CREATE INDEX IF NOT EXISTS sports_payments_live_wibday_idx
  ON public.sports_payments
     (tenant_id, ((paid_at AT TIME ZONE 'Asia/Jakarta')::date) DESC)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sports_payments_live_recorder_idx
  ON public.sports_payments
     (tenant_id, recorded_by, ((paid_at AT TIME ZONE 'Asia/Jakarta')::date) DESC)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sports_kantin_sales_live_recorder_idx
  ON public.sports_kantin_sales
     (tenant_id, recorded_by, ((created_at AT TIME ZONE 'Asia/Jakarta')::date) DESC)
  WHERE voided_at IS NULL;

-- Trigger support: the payment_status recalc SUMs amounts per booking
-- over live rows only. Index-only with this; heap visits without it.
CREATE INDEX IF NOT EXISTS sports_payments_booking_live_idx
  ON public.sports_payments (booking_id, amount)
  WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- §3. Tombstone/sort partials on user-visible lists. Every read filters
-- deleted_at IS NULL (tombstones are never reclaimed by design), and no
-- index is partial on it — tombstoned rows stay in every scan forever.
-- ---------------------------------------------------------------------------

CREATE INDEX IF NOT EXISTS sports_expenses_live_spent_idx
  ON public.sports_expenses (tenant_id, spent_at DESC)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sports_customers_live_name_idx
  ON public.sports_customers (tenant_id, name)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sports_members_live_name_idx
  ON public.sports_members (tenant_id, name)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sports_kantin_products_live_name_idx
  ON public.sports_kantin_products (tenant_id, name)
  WHERE is_active;
CREATE INDEX IF NOT EXISTS sports_facilities_live_sort_idx
  ON public.sports_facilities (tenant_id, sort_order, name)
  WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- §4. Queue/reminder/usage indexes + BRIN on append-only ledgers.
-- ---------------------------------------------------------------------------

-- Notify claim orders by id but the only index is on vt: every claim
-- sorts the whole live queue. This delivers id order; the vt index
-- stays for any_notifications_queued().
CREATE INDEX IF NOT EXISTS notify_jobs_ready_order_idx
  ON public.notify_jobs (id)
  WHERE archived_at IS NULL;

-- The retention janitor's sent/dead deletes filter created_at, but the
-- only indexes are on remind_at (pending) and claimed_at (sending):
-- every sweep reads every sent row ever written.
CREATE INDEX IF NOT EXISTS agent_reminders_sent_created_idx
  ON public.agent_reminders (created_at)
  WHERE status IN ('sent', 'dead');

-- Quota + admin usage aggregate over (client_id, created_at) and read
-- tokens/cost: index-only with the INCLUDE columns.
CREATE INDEX IF NOT EXISTS usage_client_created_include_idx
  ON public.usage (client_id, created_at DESC)
  INCLUDE (tokens_in, tokens_out, cost_usd);

-- BRIN for append-only, time-correlated ledgers: within a few percent
-- of a btree at ~1/1000th the size and ~1/50th the write cost.
CREATE INDEX IF NOT EXISTS usage_created_brin
  ON public.usage USING brin (created_at) WITH (pages_per_range = 32);
CREATE INDEX IF NOT EXISTS events_ts_brin
  ON public.events USING brin ("timestamp") WITH (pages_per_range = 32);
CREATE INDEX IF NOT EXISTS notify_jobs_vt_brin
  ON public.notify_jobs USING brin (vt) WITH (pages_per_range = 32);
CREATE INDEX IF NOT EXISTS agent_reminders_at_brin
  ON public.agent_reminders USING brin (remind_at) WITH (pages_per_range = 32);

-- ---------------------------------------------------------------------------
-- §5. Drop superseded/duplicate indexes (write amplification on the
-- three hottest write paths: usage, tenant_records, sessions).
-- Each drop verified: the surviving index serves every query the
-- dropped one served.
-- ---------------------------------------------------------------------------

-- Exact duplicates of UNIQUE constraints (Postgres already maintains
-- the constraint's own btree).
DROP INDEX IF EXISTS public.clients_token_hash_idx;
DROP INDEX IF EXISTS public.clients_tenant_id_idx;
DROP INDEX IF EXISTS public.sports_sessions_token_idx;
DROP INDEX IF EXISTS public.sports_pricing_rules_tenant_idx;
-- No query filters sessions by user_id (only token_hash + expires_at).
DROP INDEX IF EXISTS public.sports_sessions_user_idx;
-- 2 values, no query filters kind: maintained on every LLM call.
DROP INDEX IF EXISTS public.usage_kind_idx;
-- Superseded by 044's tenant-leading composite.
DROP INDEX IF EXISTS public.sports_kantin_movements_product_idx;
-- Superseded by 044's (tenant_id, sale_id): RLS adds the tenant qual.
DROP INDEX IF EXISTS public.sports_kantin_items_sale_idx;
-- ~13 values; vertical is read from the Redis client model, never SQL.
DROP INDEX IF EXISTS public.clients_vertical_idx;
-- Superseded by the §2 partial expression indexes (same expression,
-- plus the tombstone predicate every money query needs).
DROP INDEX IF EXISTS public.sports_payments_wibday_idx;
DROP INDEX IF EXISTS public.sports_kantin_sales_wibday_idx;
-- Replaced by usage_client_created_include_idx (§4): same leading
-- columns, plus index-only coverage. Atomic swap inside this tx.
DROP INDEX IF EXISTS public.usage_client_created_idx;

-- ---------------------------------------------------------------------------
-- §6. Report definer. report_objects is FORCE RLS on tenant_id =
-- current_setting('app.tenant_id'), but the runtime pool never sets the
-- GUC — so MintReportToken fails 42501 on every export and the sweeper
-- matches zero rows forever. The policy is right; the callers get a
-- definer path instead of a weaker policy. Follows the 021
-- auth_definer pattern exactly.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'report_definer') THEN
    CREATE ROLE report_definer NOLOGIN BYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA private TO report_definer;
GRANT SELECT, INSERT, DELETE ON public.report_objects TO report_definer;
GRANT report_definer TO postgres;
GRANT USAGE, CREATE ON SCHEMA private TO report_definer;

CREATE OR REPLACE FUNCTION private.mint_report(
  p_hash text, p_tenant text, p_object_key text,
  p_size integer, p_ttl_secs integer)
RETURNS boolean
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = '' AS $$
  INSERT INTO public.report_objects
         (token_hash, tenant_id, object_key, size_bytes, expires_at)
  VALUES (p_hash, p_tenant, p_object_key, p_size,
          pg_catalog.now() + make_interval(secs => p_ttl_secs))
  ON CONFLICT (token_hash) DO NOTHING;
  SELECT true;
$$;
ALTER FUNCTION private.mint_report(text, text, text, integer, integer) OWNER TO report_definer;
REVOKE ALL ON FUNCTION private.mint_report(text, text, text, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.mint_report(text, text, text, integer, integer) TO app_runtime;

CREATE OR REPLACE FUNCTION private.expired_reports(p_limit integer DEFAULT 100)
RETURNS TABLE(token_hash text, tenant_id text, object_key text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = '' AS $$
  SELECT r.token_hash, r.tenant_id, r.object_key
    FROM public.report_objects r
   WHERE r.expires_at < pg_catalog.now()
   ORDER BY r.expires_at
   LIMIT GREATEST(p_limit, 0);
$$;
ALTER FUNCTION private.expired_reports(integer) OWNER TO report_definer;
REVOKE ALL ON FUNCTION private.expired_reports(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.expired_reports(integer) TO app_runtime;

CREATE OR REPLACE FUNCTION private.delete_reports(p_hashes text[])
RETURNS integer
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = '' AS $$
  WITH del AS (
    DELETE FROM public.report_objects r
     WHERE r.token_hash = ANY(p_hashes)
    RETURNING 1
  ) SELECT COUNT(*)::int FROM del;
$$;
ALTER FUNCTION private.delete_reports(text[]) OWNER TO report_definer;
REVOKE ALL ON FUNCTION private.delete_reports(text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.delete_reports(text[]) TO app_runtime;

-- ---------------------------------------------------------------------------
-- §7. Quota definer. usage_read_runtime grants app_runtime an
-- unqualified SELECT over all of usage — the highest-cardinality table
-- in the system — so any app-pool SQL can SUM every tenant's ledger.
-- Collapse the surface to one function that can only ever see one
-- client's rows, and drop the blanket policy (keep the INSERT one).
-- ---------------------------------------------------------------------------

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'quota_reader') THEN
    CREATE ROLE quota_reader NOLOGIN BYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA private TO quota_reader;
GRANT SELECT (client_id, tokens_in, tokens_out, created_at) ON public.usage TO quota_reader;
GRANT quota_reader TO postgres;
GRANT USAGE, CREATE ON SCHEMA private TO quota_reader;

CREATE OR REPLACE FUNCTION private.month_tokens(p_client uuid)
RETURNS integer
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = '' AS $$
  SELECT COALESCE(SUM(tokens_in + tokens_out), 0)::int
    FROM public.usage
   WHERE client_id = p_client
     AND created_at >= pg_catalog.date_trunc('month', pg_catalog.now());
$$;
ALTER FUNCTION private.month_tokens(uuid) OWNER TO quota_reader;
REVOKE ALL ON FUNCTION private.month_tokens(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.month_tokens(uuid) TO app_runtime;

DROP POLICY IF EXISTS usage_read_runtime ON public.usage;

-- ---------------------------------------------------------------------------
-- §8. Booking-guard advisory lock. The partial EXCLUDE constraint cannot
-- be satisfied from its index under RLS, so concurrent inserts to the
-- same court-day re-check on the heap and deadlock (see 02eb461: the
-- retry is correct but triples load exactly when the DB is hot). One
-- advisory lock per (tenant, facility, date) — taken in the BEFORE
-- trigger, i.e. before any row lock — turns the deadlock into a short
-- wait. Released automatically at commit/rollback (xact scope), so a
-- crash can never leave it held. The 40P01 retry stays as a safety net.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION sports_blocked_booking_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
AS $$
DECLARE
  v_reason text;
BEGIN
  -- Serialize concurrent writers to one court-day before the EXCLUDE
  -- check runs. Always taken before any row lock on this path, so no
  -- lock-order cycle is possible (nothing else takes this lock).
  PERFORM pg_advisory_xact_lock(
    pg_catalog.hashtextextended(
      NEW.tenant_id || ':' || NEW.facility_id::text || ':' || NEW.date::text, 0::bigint));

  -- Half-open overlap: a booking may start exactly when a block ends.
  -- A block of 09:00-12:00 must not refuse a 12:00-13:00 booking.
  SELECT b.reason INTO v_reason
    FROM sports_court_blocks b
   WHERE b.tenant_id = NEW.tenant_id
     AND b.block_date = NEW.date
     AND (b.facility_id = NEW.facility_id OR b.facility_id IS NULL)
     AND b.start_time < NEW.end_time
     AND b.end_time   > NEW.start_time
   LIMIT 1;

  IF v_reason IS NOT NULL THEN
    RAISE EXCEPTION
      'slot is blocked (%): court % on % %-%',
      v_reason, NEW.facility_id, NEW.date,
      to_char(NEW.start_time, 'HH24:MI'), to_char(NEW.end_time, 'HH24:MI')
      USING ERRCODE = 'exclusion_violation';
  END IF;
  RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- §9. Payment-status no-op-write guards. Both recalc functions rewrote
-- the sports_bookings row on every payment write even when the status
-- did not change: a fresh heap tuple + every-index update + WAL +
-- autovacuum work, per payment, on the busiest table. IS DISTINCT FROM
-- makes the UPDATE a no-op when already correct. Logic unchanged.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION sports_recalc_payment_status()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
AS $$
DECLARE
  v_booking_id uuid := COALESCE(NEW.booking_id, OLD.booking_id);
  v_price numeric;
  v_paid numeric;
  v_status text;
BEGIN
  IF v_booking_id IS NULL THEN
    RETURN NULL;
  END IF;
  SELECT COALESCE(override_price, price) INTO v_price
    FROM sports_bookings WHERE id = v_booking_id;
  SELECT COALESCE(SUM(amount),0) INTO v_paid
    FROM sports_payments WHERE booking_id = v_booking_id AND deleted_at IS NULL;

  v_status := CASE
    WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'
    WHEN v_paid >= v_price                THEN 'paid'
    WHEN v_paid > 0                       THEN 'partial'
    ELSE 'unpaid' END;

  -- Only write when it actually changes: a no-op UPDATE still mints a
  -- row version, rewrites every index on sports_bookings, and emits
  -- WAL (plus any future AFTER UPDATE trigger would fire on it too).
  UPDATE sports_bookings SET payment_status = v_status
   WHERE id = v_booking_id AND payment_status IS DISTINCT FROM v_status;
  RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION sports_recalc_booking_payment_status()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
AS $$
DECLARE
  v_booking_id uuid := NEW.id;
  v_price numeric;
  v_paid numeric;
  v_status text;
BEGIN
  IF TG_OP = 'UPDATE'
     AND NEW.override_price IS NOT DISTINCT FROM OLD.override_price
     AND NEW.price               IS NOT DISTINCT FROM OLD.price THEN
    RETURN NULL;
  END IF;

  SELECT COALESCE(override_price, price) INTO v_price
    FROM sports_bookings WHERE id = v_booking_id;
  SELECT COALESCE(SUM(amount), 0) INTO v_paid
    FROM sports_payments WHERE booking_id = v_booking_id AND deleted_at IS NULL;

  v_status := CASE
    WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'
    WHEN v_paid >= v_price                THEN 'paid'
    WHEN v_paid > 0                       THEN 'partial'
    ELSE 'unpaid' END;

  UPDATE sports_bookings SET payment_status = v_status
   WHERE id = v_booking_id AND payment_status IS DISTINCT FROM v_status;
  RETURN NULL;
END;
$$;
