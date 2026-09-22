-- 003_seed_dev.sql
-- token = 'dev-token', hash = sha256Hex('dev-token')
-- DONE: PRD 4.3
INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
VALUES ('dev', 'dev', encode(digest('dev-token','sha256'),'hex'), 1000000, 'active')
ON CONFLICT (tenant_id) DO NOTHING;
