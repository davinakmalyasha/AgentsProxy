-- 0007_staff_payments_to_project.sql — Centrum payroll belongs in Centrum.
--
-- public.agent_staff_payments was created in the platform schema before the
-- project-schema split. It is Centrum business data, not platform state, so it
-- moves beside the rest of Centrum's tables. The platform table stays for one
-- release; 0008 drops it after every binary uses this table.
--
-- Additive: create + copy. The copy is idempotent by primary key.

CREATE TABLE IF NOT EXISTS project_centrum.sports_staff_payments (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES public.clients(tenant_id) ON DELETE CASCADE,
  staff_name text NOT NULL,
  amount     numeric(12,2) NOT NULL,
  paid_at    date NOT NULL DEFAULT CURRENT_DATE,
  note       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_staff_payments_tenant_idx
  ON project_centrum.sports_staff_payments (tenant_id, paid_at);

ALTER TABLE project_centrum.sports_staff_payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_centrum.sports_staff_payments FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_staff_payments_isolation ON project_centrum.sports_staff_payments;
CREATE POLICY sports_staff_payments_isolation ON project_centrum.sports_staff_payments
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT ON project_centrum.sports_staff_payments TO app_runtime;

DO $$
BEGIN
  IF to_regclass('public.agent_staff_payments') IS NOT NULL THEN
    INSERT INTO project_centrum.sports_staff_payments
      (id, tenant_id, staff_name, amount, paid_at, note, created_at)
    SELECT id, tenant_id, staff_name, amount, paid_at, note, created_at
      FROM public.agent_staff_payments
    ON CONFLICT (id) DO NOTHING;
  END IF;
END $$;

COMMENT ON TABLE project_centrum.sports_staff_payments IS
  'Centrum staff payroll ledger. Project business data; the platform agent state stays in public.';
