-- 016_sports_expenses.sql — Phase 6: owner expense ledger.
-- Port of CentrumBadminton's expenses (owner-only, categories).
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_expenses (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  category   text NOT NULL DEFAULT 'lainnya',
  amount     numeric(12,2) NOT NULL CHECK (amount > 0),
  note       text NOT NULL DEFAULT '',
  spent_at   date NOT NULL DEFAULT CURRENT_DATE,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_expenses_tenant_spent_idx
  ON sports_expenses(tenant_id, spent_at);

ALTER TABLE sports_expenses ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_expenses FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_expenses_isolation ON sports_expenses;
CREATE POLICY sports_expenses_isolation ON sports_expenses
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_expenses TO app_runtime;
