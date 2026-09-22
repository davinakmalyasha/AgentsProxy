-- 011_agent_bindings.sql — Phase 5: Telegram chat → client binding.
-- DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
--
-- Links one Telegram chat_id to exactly one client. The bot resolves
-- incoming messages through this table, then gates on the client's
-- `features` containing "telegram". Admin binds a chat to a client
-- as a one-time onboarding step.

CREATE TABLE IF NOT EXISTS agent_bindings (
  chat_id    text PRIMARY KEY,
  client_id  uuid NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  status     text NOT NULL DEFAULT 'active',   -- active | disabled
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_bindings_client_idx ON agent_bindings(client_id);

ALTER TABLE agent_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_bindings FORCE ROW LEVEL SECURITY;

-- Bindings are cross-tenant by design (chat → client lookup), so the
-- RLS policy allows the app_runtime role to read any binding. The
-- binding row is the *key*, not tenant data; the tenant boundary is
-- enforced by the client's own feature gate + the tool layer's
-- WithTenantTx. Admin (superuser) manages bindings.
DROP POLICY IF EXISTS agent_bindings_read_all ON agent_bindings;
CREATE POLICY agent_bindings_read_all ON agent_bindings
  FOR SELECT
  USING (true);

GRANT SELECT ON agent_bindings TO app_runtime;
