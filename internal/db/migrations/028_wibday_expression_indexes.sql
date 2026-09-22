-- 028_wibday_expression_indexes.sql — let WIB calendar-day predicates
-- use an index. Money/report queries filter with
--   (paid_at AT TIME ZONE 'Asia/Jakarta')::date BETWEEN ...
--   (created_at AT TIME ZONE 'Asia/Jakarta')::date ...
-- which Postgres cannot satisfy with the plain (tenant_id, paid_at) /
-- (tenant_id, created_at) btree. Expression indexes on the same
-- function make those scans index-assisted as history grows.
-- DONE: DB query optimization / STANDARDS A

CREATE INDEX IF NOT EXISTS sports_payments_wibday_idx
    ON sports_payments (tenant_id, ((paid_at AT TIME ZONE 'Asia/Jakarta')::date));

CREATE INDEX IF NOT EXISTS sports_kantin_sales_wibday_idx
    ON sports_kantin_sales (tenant_id, ((created_at AT TIME ZONE 'Asia/Jakarta')::date));
