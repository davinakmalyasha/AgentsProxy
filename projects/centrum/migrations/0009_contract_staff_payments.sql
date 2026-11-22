-- 0009_contract_staff_payments.sql — Centrum payroll has one home.
--
-- 0007 copied the ledger into project_centrum.sports_staff_payments and every
-- current binary writes there. The public table is the temporary expand-side
-- copy. Before dropping it, copy any row an old instance wrote during the
-- deploy window, then verify nothing was left behind.
--
-- This is a project migration, not a platform migration, because the
-- migration runner applies platform migrations before project migrations.
-- Platform 013 creates public.agent_staff_payments and Centrum 0007 fills the
-- project table; the final cleanup has to run after both.
--
-- Contract step: destructive by design. Apply only after the stability window.

DO $$
DECLARE
  v_missing int;
BEGIN
  IF to_regclass('public.agent_staff_payments') IS NOT NULL THEN
    INSERT INTO project_centrum.sports_staff_payments
      (id, tenant_id, staff_name, amount, paid_at, note, created_at)
    SELECT id, tenant_id, staff_name, amount, paid_at, note, created_at
      FROM public.agent_staff_payments
    ON CONFLICT (id) DO NOTHING;

    SELECT count(*) INTO v_missing
      FROM public.agent_staff_payments p
     WHERE NOT EXISTS (
       SELECT 1
         FROM project_centrum.sports_staff_payments s
        WHERE s.id = p.id
     );
    IF v_missing > 0 THEN
      RAISE EXCEPTION 'staff payments copy incomplete: % rows missing', v_missing;
    END IF;
  END IF;
END $$;

DROP TABLE IF EXISTS public.agent_staff_payments;

COMMENT ON TABLE project_centrum.sports_staff_payments IS
  'Centrum staff payroll ledger. The one payroll home after the legacy public table was retired.';
