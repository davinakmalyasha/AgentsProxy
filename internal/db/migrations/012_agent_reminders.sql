-- 012_agent_reminders.sql — Phase 5: agentic reminders.
-- DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
--
-- "Remind me Friday 8am to order shuttlecocks" → the agent inserts a
-- row here; the reminder scheduler polls due rows and sends via
-- Telegram. Tenant-scoped like every other table.

CREATE TABLE IF NOT EXISTS agent_reminders (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  chat_id    text NOT NULL DEFAULT '',
  text       text NOT NULL,
  remind_at  timestamptz NOT NULL,
  status     text NOT NULL DEFAULT 'pending',  -- pending | sent | cancelled
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_reminders_due_idx
  ON agent_reminders(status, remind_at);

ALTER TABLE agent_reminders ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_reminders FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS agent_reminders_isolation ON agent_reminders;
CREATE POLICY agent_reminders_isolation ON agent_reminders
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_reminders TO app_runtime;
