-- 056_range_summary.sql — one-RPC money dashboard.
--
-- query_range_summary ran 7 round trips inside a transaction (BEGIN +
-- set_config + LoadBasis×3 + expenses + bookings + best-day + peak-hours +
-- COMMIT = 9 RTs); query_net_profit 6, query_income 5. All three tools are
-- read-only aggregates over the same four ledgers, so collapse them into a
-- single STABLE definer that returns every aggregate as one jsonb row:
-- 9/6/5 RTs -> 1, with no transaction at all.
--
-- Every subquery below is transcribed verbatim from the Go it replaces
-- (AgentsTools.LoadBasis + tenants/centrum-badminton/tools/query_*):
-- same WIB conversions, same deleted_at/voided_at filters, same status
-- sets, same GROUP BY/ORDER BY/LIMIT shapes. The only deliberate change is
-- peak hours ordered by hour instead of by count — the Go renderer iterates
-- h=0..23 and re-sorts by hour anyway, so the output string is identical.
--
-- Numeric casts: SUM() is numeric; ::float8 keeps the JSON payload small.
-- Both numeric->float64 (old pgx scan) and numeric->float8->JSON->float64
-- (new path) are correctly-rounded, so whole-rupiah amounts are bit-identical.
-- COUNT(*)::int: booking/payment counts fit int; the Go side scans int.
--
-- Isolation: SECURITY DEFINER as money_reader (BYPASSRLS, minimal table
-- grants, same pattern as report_definer/quota_reader in 053). Tenant
-- scoping is the p_tenant predicate present in EVERY subquery — a new
-- aggregate added here without it would read all tenants, so keep them
-- uniform. REVOKE ALL FROM PUBLIC + GRANT EXECUTE TO app_runtime, the only
-- runtime caller (agent tools run as app_runtime or superuser).
--
-- Additive-only: one new role, one new function. No table touched.

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'money_reader') THEN
    CREATE ROLE money_reader NOLOGIN BYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA private TO money_reader;
GRANT SELECT ON public.sports_payments TO money_reader;
GRANT SELECT ON public.sports_bookings TO money_reader;
GRANT SELECT ON public.sports_kantin_sales TO money_reader;
GRANT SELECT ON public.sports_expenses TO money_reader;
GRANT money_reader TO postgres;
GRANT USAGE, CREATE ON SCHEMA private TO money_reader;

CREATE OR REPLACE FUNCTION private.range_summary(p_tenant text, p_from date, p_to date)
RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = '' AS $$
  -- Best day is the day money actually came in (paid_at), not booking value.
  -- One CTE, referenced twice: two separate GROUP BY evaluations could
  -- disagree on a tied day, while one CTE under a single snapshot cannot.
  WITH best AS (
    SELECT ((p.paid_at AT TIME ZONE 'Asia/Jakarta')::date)::text AS day,
           SUM(p.amount)::float8 AS amt
      FROM public.sports_payments p
     WHERE p.tenant_id = p_tenant
       AND p.deleted_at IS NULL
       AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
       AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to
     GROUP BY 1 ORDER BY SUM(p.amount) DESC LIMIT 1
  )
  SELECT jsonb_build_object(
    -- LoadBasis Q1: received split (cash/qris/other) + payments counted.
    'cash',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'cash'), 0)::float8
                FROM public.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'qris',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'qris'), 0)::float8
                FROM public.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'other', (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method NOT IN ('cash', 'qris')), 0)::float8
                FROM public.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'count', (SELECT COUNT(*)::int
                FROM public.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    -- LoadBasis Q2: committed value, excluding slots never charged.
    'booked', (SELECT COALESCE(SUM(COALESCE(b.override_price, b.price)), 0)::float8
                 FROM public.sports_bookings b
                WHERE b.tenant_id = p_tenant
                  AND b.date >= p_from AND b.date < p_to
                  AND b.status IN ('confirmed', 'checked_in', 'completed')),
    -- LoadBasis Q3: canteen sales; a voided sale is a correction, not revenue.
    'kantin', (SELECT COALESCE(SUM(k.total), 0)::float8
                 FROM public.sports_kantin_sales k
                WHERE k.tenant_id = p_tenant
                  AND k.voided_at IS NULL
                  AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                  AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    -- Expenses: spent_at is a plain date (no WIB conversion).
    'expenses', (SELECT COALESCE(SUM(e.amount), 0)::float8
                   FROM public.sports_expenses e
                  WHERE e.tenant_id = p_tenant
                    AND e.deleted_at IS NULL
                    AND e.spent_at >= p_from AND e.spent_at < p_to),
    -- Booking headcount (cancelled/no-show never happened).
    'bookings', (SELECT COUNT(*)::int
                   FROM public.sports_bookings b
                  WHERE b.tenant_id = p_tenant
                    AND b.date >= p_from AND b.date < p_to
                    AND b.status NOT IN ('cancelled', 'no_show')),
    -- Best day is the day money actually came in: single CTE above.
    -- The outer COALESCE is load-bearing: when best is empty the scalar
    -- subquery yields no row (NULL); COALESCE inside would never fire.
    'best_day', (SELECT day FROM best),
    'best_day_amt', COALESCE((SELECT amt FROM best), 0),
    -- Peak hours from booking start times; ordered by hour (Go renders 0..23).
    'peak', COALESCE((SELECT jsonb_agg(jsonb_build_array(h, n) ORDER BY h)
                        FROM (SELECT EXTRACT(HOUR FROM b.start_time)::int AS h, COUNT(*)::int AS n
                                FROM public.sports_bookings b
                               WHERE b.tenant_id = p_tenant
                                 AND b.date >= p_from AND b.date < p_to
                                 AND b.status NOT IN ('cancelled', 'no_show')
                               GROUP BY 1 ORDER BY 2 DESC LIMIT 3) t),
                     '[]'::jsonb)
  );
$$;
ALTER FUNCTION private.range_summary(text, date, date) OWNER TO money_reader;
REVOKE ALL ON FUNCTION private.range_summary(text, date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.range_summary(text, date, date) TO app_runtime;
