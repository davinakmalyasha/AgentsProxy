-- 0006_grant_money_reader_clients.sql — let the money definer resolve schemas.
--
-- 0004 made private.range_summary resolve the tenant's schema from
-- clients.db_schema, but money_reader (the SECURITY DEFINER owner) was only
-- granted on the project tables. The first agent query then failed with
-- "permission denied for table clients". Grant exactly the columns the
-- resolver needs.

GRANT SELECT (tenant_id, db_schema, status) ON public.clients TO money_reader;
GRANT SELECT (tenant_id, db_schema, status) ON public.clients TO auth_definer;
