-- 004_features.sql — Phase 3: per-vertical, per-client feature toggles.
-- DONE: PRD 6.5 (feature platform) / STANDARDS A, H

ALTER TABLE clients ADD COLUMN IF NOT EXISTS vertical text NOT NULL DEFAULT '';
ALTER TABLE clients ADD COLUMN IF NOT EXISTS features text[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS clients_vertical_idx ON clients(vertical);

-- Backfill the dev row with the default Phase 3 feature set (idempotent).
UPDATE clients
   SET vertical = 'fnb',
       features = ARRAY['records','menu','booking','review','article','promo']
 WHERE tenant_id = 'dev' AND features = '{}';
