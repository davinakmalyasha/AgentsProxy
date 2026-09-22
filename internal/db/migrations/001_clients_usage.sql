-- 001_clients_usage.sql
-- DONE: PRD 4.1
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS schema_migrations (
  version text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS clients (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name            text NOT NULL,
  tenant_id       text NOT NULL UNIQUE,
  token_hash      text NOT NULL UNIQUE,
  monthly_quota_tokens integer NOT NULL DEFAULT 500000,
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','blocked')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS clients_token_hash_idx ON clients(token_hash);
CREATE INDEX IF NOT EXISTS clients_tenant_id_idx ON clients(tenant_id);

CREATE TABLE IF NOT EXISTS usage (
  id              bigserial PRIMARY KEY,
  client_id       uuid NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  tokens_in       integer NOT NULL DEFAULT 0,
  tokens_out      integer NOT NULL DEFAULT 0,
  cost_usd        numeric(10,6) NOT NULL DEFAULT 0,
  model           text NOT NULL DEFAULT '',
  kind            text NOT NULL CHECK (kind IN ('chat','business')),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS usage_client_created_idx ON usage(client_id, created_at);
CREATE INDEX IF NOT EXISTS usage_created_idx ON usage(created_at);
CREATE INDEX IF NOT EXISTS usage_kind_idx ON usage(kind);
