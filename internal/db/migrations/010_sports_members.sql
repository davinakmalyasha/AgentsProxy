-- 010_sports_members.sql — sports vertical: members with prepaid
-- balance.
-- DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
--
-- Custom table (not tenant_records) because a prepaid `balance`
-- needs a NUMERIC column with atomic top-ups.

CREATE TABLE IF NOT EXISTS sports_members (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name       text NOT NULL,
  phone      text NOT NULL DEFAULT '',
  balance    numeric(10,2) NOT NULL DEFAULT 0,
  status     text NOT NULL DEFAULT 'active', -- active | paused
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_members_tenant_idx ON sports_members(tenant_id);

ALTER TABLE sports_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_members FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_members_isolation ON sports_members;
CREATE POLICY sports_members_isolation ON sports_members
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_members TO app_runtime;
