-- 013_agent_staff_payments.sql — Phase 5: staff payment LEDGER.
-- DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
--
-- LEDGER ONLY — records who got paid what, when. No money movement.
-- Real payouts would be a later phase with a payment provider.

CREATE TABLE IF NOT EXISTS agent_staff_payments (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  staff_name text NOT NULL,
  amount     numeric(12,2) NOT NULL,
  paid_at    date NOT NULL DEFAULT CURRENT_DATE,
  note       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_staff_payments_tenant_idx
  ON agent_staff_payments(tenant_id, paid_at);

ALTER TABLE agent_staff_payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_staff_payments FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS agent_staff_payments_isolation ON agent_staff_payments;
CREATE POLICY agent_staff_payments_isolation ON agent_staff_payments
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_staff_payments TO app_runtime;
