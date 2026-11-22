-- 082_revoke_agency_runtime_grants.sql — the agency plane is not tenant runtime.
--
-- 068/070/072 granted app_runtime full DML on the DFD agency tables. The
-- agency router actually runs on the admin pool (internal/api/router.go), so
-- these grants were never needed. They are an unnecessary escalation path:
-- any SQL that can run as app_runtime could mint an agency API key or reset a
-- SUPERADMIN.
--
-- Revoke is idempotent, and the regclass guard keeps the migration safe if an
-- agency table is ever retired before this file runs. The tenant runtime still
-- keeps its public tenant tables, project schemas, and the private definer
-- functions it needs.

DO $$
DECLARE
  t text;
  agency_tables text[] := ARRAY[
    'users', 'leads', 'packages', 'promos', 'orders', 'payments',
    'order_notes', 'order_resources', 'projects', 'project_images',
    'hosting', 'articles', 'contracts', 'order_milestones',
    'milestone_views', 'order_activities', 'client_requests',
    'audit_logs', 'global_settings', 'api_keys', 'agent_events',
    'webhook_subscriptions', 'webhook_deliveries', 'idempotency_keys',
    'login_otps', 'signup_requests'
  ];
  agency_sequences text[] := ARRAY[
    'users_id_seq', 'leads_id_seq', 'packages_id_seq', 'promos_id_seq',
    'order_notes_id_seq', 'order_resources_id_seq', 'projects_id_seq',
    'project_images_id_seq', 'hosting_id_seq', 'articles_id_seq',
    'order_milestones_id_seq', 'milestone_views_id_seq',
    'order_activities_id_seq', 'client_requests_id_seq',
    'audit_logs_id_seq', 'global_settings_id_seq', 'agent_events_id_seq',
    'webhook_deliveries_id_seq', 'login_otps_id_seq',
    'signup_requests_id_seq'
  ];
BEGIN
  FOREACH t IN ARRAY agency_tables LOOP
    IF to_regclass('public.' || t) IS NOT NULL THEN
      EXECUTE format('REVOKE ALL PRIVILEGES ON TABLE public.%I FROM app_runtime', t);
    END IF;
  END LOOP;
  FOREACH t IN ARRAY agency_sequences LOOP
    IF to_regclass('public.' || t) IS NOT NULL THEN
      EXECUTE format('REVOKE ALL PRIVILEGES ON SEQUENCE public.%I FROM app_runtime', t);
    END IF;
  END LOOP;
END $$;

COMMENT ON SCHEMA public IS
  'Platform tables for tenant traffic plus the DFD agency plane. app_runtime is intentionally not granted agency DML; agency routes use the admin pool.';
