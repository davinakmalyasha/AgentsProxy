-- 025_telegram_bots.sql — T1: multi-bot Telegram (1 bot per role per tenant).
-- DONE: T1 (multi-bot model)
--
-- One global TELEGRAM_BOT_TOKEN cannot scale to thousands of tenants,
-- and one bot cannot serve three roles safely. Bot tokens live HERE
-- (AES-GCM encrypted, one TELEGRAM_VAULT_KEY in env) — never in .env.
--
-- What changes:
--   telegram_bots   new registry: tenant + fixed role + encrypted token
--                   + per-bot webhook secret + chat allowlist + persisted
--                   update offset (fixes replay-on-restart).
--   agent_bindings  chat→client becomes (bot, chat)→client: the same
--                   person can talk to a tenant's owner-bot AND client-bot.
--                   Legacy env-bot rows are dropped (legacy bot removed).
--   agent_reminders bot_id so the scheduler sends through the OWNING bot.

CREATE TABLE IF NOT EXISTS telegram_bots (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name            text NOT NULL DEFAULT '',
  role            text NOT NULL DEFAULT 'client' CHECK (role IN ('owner','pegawai','client')),
  token_enc       bytea NOT NULL,
  key_ver         smallint NOT NULL DEFAULT 1,
  webhook_secret  text NOT NULL DEFAULT '',
  allow_chat_ids  bigint[] NOT NULL DEFAULT '{}',
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  last_update_id  bigint NOT NULL DEFAULT 0,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS telegram_bots_tenant_idx ON telegram_bots(tenant_id);
CREATE INDEX IF NOT EXISTS telegram_bots_status_idx ON telegram_bots(status);

ALTER TABLE telegram_bots ENABLE ROW LEVEL SECURITY;
ALTER TABLE telegram_bots FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS telegram_bots_isolation ON telegram_bots;
CREATE POLICY telegram_bots_isolation ON telegram_bots
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON telegram_bots TO app_runtime;

-- (Phase 1.4: the agent_bindings ALTER block lived here. 011 is retired
-- and the table is never created, so the block was removed — ALTER on a
-- missing table would abort the chain.)

-- Reminders: remember which bot owns the chat.
ALTER TABLE agent_reminders
  ADD COLUMN IF NOT EXISTS bot_id uuid REFERENCES telegram_bots(id) ON DELETE SET NULL;
