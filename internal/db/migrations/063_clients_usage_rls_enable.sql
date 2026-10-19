-- 063_clients_usage_rls_enable.sql — make the clients/usage RLS real in
-- every environment.
--
-- 033 added the policies for these two tables but deliberately never
-- ENABLED row security: production had RLS enabled out-of-band, so the
-- policies applied there while a database built purely from migrations
-- had RLS OFF and only table grants governed. That never breaks the app
-- (the policies are permissive USING (true) for app_runtime), but a
-- fresh environment — including any future Supabase project — would ship
-- publicly exposed tables with no row security, which is the one thing
-- the Supabase API layer relies on.
--
-- Enabled WITHOUT FORCE, matching prod: the owner (admin pool) must keep
-- writing these tables for provisioning, and FORCE would apply policies
-- to the owner too (none target it) — owner writes would break.
--
-- Policies re-asserted verbatim from 033 so this file is self-contained
-- for anyone reading the row-security surface of these two tables.

ALTER TABLE public.clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.usage ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS clients_read_runtime ON public.clients;
CREATE POLICY clients_read_runtime ON public.clients
  FOR SELECT
  TO app_runtime
  USING (true);

DROP POLICY IF EXISTS usage_read_runtime ON public.usage;
CREATE POLICY usage_read_runtime ON public.usage
  FOR SELECT
  TO app_runtime
  USING (true);

DROP POLICY IF EXISTS usage_write_runtime ON public.usage;
CREATE POLICY usage_write_runtime ON public.usage
  FOR INSERT
  TO app_runtime
  WITH CHECK (true);
