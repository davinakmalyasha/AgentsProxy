-- 0008_money_summary_project.sql — money summary is Centrum business logic.
--
-- private.range_summary was a platform definer that knew Centrum's tables and
-- needed money_reader privileges across them. It moves here as an invoker
-- function: the caller runs inside tenantdb.WithTx, so app_runtime's RLS
-- policies apply and a cloned schema resolves exactly like project_centrum.
--
-- Additive: new project function + grant. The old private function remains
-- until the contract migration.

CREATE OR REPLACE FUNCTION project_centrum.money_range_summary(
  p_tenant text,
  p_from   date,
  p_to     date
)
RETURNS jsonb
LANGUAGE plpgsql
STABLE
SET search_path = ''
AS $function$
DECLARE
  v_schema text;
  v_out    jsonb;
BEGIN
  SELECT c.db_schema INTO v_schema
    FROM public.clients c
   WHERE c.tenant_id = p_tenant;
  IF v_schema IS NULL OR v_schema !~ '^project_[a-z0-9_]+$' THEN
    RETURN jsonb_build_object(
      'cash', 0, 'qris', 0, 'other', 0, 'count', 0,
      'booked', 0, 'kantin', 0, 'expenses', 0, 'bookings', 0,
      'best_day', NULL, 'best_day_amt', 0, 'peak', '[]'::jsonb);
  END IF;
  IF to_regclass(pg_catalog.format('%I.sports_payments', v_schema)) IS NULL THEN
    RETURN jsonb_build_object(
      'cash', 0, 'qris', 0, 'other', 0, 'count', 0,
      'booked', 0, 'kantin', 0, 'expenses', 0, 'bookings', 0,
      'best_day', NULL, 'best_day_amt', 0, 'peak', '[]'::jsonb);
  END IF;
  IF NOT has_table_privilege(current_user,
        pg_catalog.format('%I.sports_payments', v_schema), 'SELECT') THEN
    RETURN jsonb_build_object(
      'cash', 0, 'qris', 0, 'other', 0, 'count', 0,
      'booked', 0, 'kantin', 0, 'expenses', 0, 'bookings', 0,
      'best_day', NULL, 'best_day_amt', 0, 'peak', '[]'::jsonb);
  END IF;

  EXECUTE pg_catalog.format($query$
    WITH best AS (
      SELECT ((p.paid_at AT TIME ZONE 'Asia/Jakarta')::date)::text AS day,
             SUM(p.amount)::float8 AS amt
        FROM %I.sports_payments p
       WHERE p.tenant_id = $1
         AND p.deleted_at IS NULL
         AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
         AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3
       GROUP BY 1 ORDER BY SUM(p.amount) DESC LIMIT 1
    )
    SELECT jsonb_build_object(
      'cash',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'cash'), 0)::float8
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'qris',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'qris'), 0)::float8
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'other', (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method NOT IN ('cash', 'qris')), 0)::float8
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'count', (SELECT COUNT(*)::int
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'booked', (SELECT COALESCE(SUM(COALESCE(b.override_price, b.price)), 0)::float8
                   FROM %I.sports_bookings b
                  WHERE b.tenant_id = $1
                    AND b.date >= $2 AND b.date < $3
                    AND b.status IN ('confirmed','checked_in','completed')),
      'kantin', (SELECT COALESCE(SUM(k.total), 0)::float8
                   FROM %I.sports_kantin_sales k
                  WHERE k.tenant_id = $1
                    AND k.voided_at IS NULL
                    AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                    AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'expenses', (SELECT COALESCE(SUM(e.amount), 0)::float8
                     FROM %I.sports_expenses e
                    WHERE e.tenant_id = $1
                      AND e.deleted_at IS NULL
                      AND e.spent_at >= $2 AND e.spent_at < $3),
      'bookings', (SELECT COUNT(*)::int
                     FROM %I.sports_bookings b
                    WHERE b.tenant_id = $1
                      AND b.date >= $2 AND b.date < $3
                      AND b.status NOT IN ('cancelled','no_show')),
      'best_day', (SELECT day FROM best),
      'best_day_amt', COALESCE((SELECT amt FROM best), 0),
      'peak', COALESCE((SELECT jsonb_agg(jsonb_build_array(h, n) ORDER BY h)
                          FROM (SELECT EXTRACT(HOUR FROM b.start_time)::int AS h, COUNT(*)::int AS n
                                  FROM %I.sports_bookings b
                                 WHERE b.tenant_id = $1
                                   AND b.date >= $2 AND b.date < $3
                                   AND b.status NOT IN ('cancelled','no_show')
                                 GROUP BY 1 ORDER BY 2 DESC LIMIT 3) t),
                       '[]'::jsonb)
    );
  $query$,
    v_schema, v_schema, v_schema, v_schema, v_schema, v_schema,
    v_schema, v_schema, v_schema, v_schema)
    INTO v_out
    USING p_tenant, p_from, p_to;
  RETURN v_out;
END;
$function$;

REVOKE ALL ON FUNCTION project_centrum.money_range_summary(text, date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION project_centrum.money_range_summary(text, date, date) TO app_runtime;

COMMENT ON FUNCTION project_centrum.money_range_summary(text, date, date) IS
  'Centrum money aggregate. SECURITY INVOKER: call inside tenantdb.WithTx so RLS and the tenant schema apply.';
