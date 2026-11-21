-- 0003_force_rls_kantin.sql — close the FORCE RLS gap.
--
-- These three tables were created with ENABLE ROW LEVEL SECURITY but never
-- FORCE. app_runtime is not the table owner, so tenant traffic is still
-- filtered, but owner/definer paths and the schema contract were weaker than
-- every other project table. Make the invariant uniform.

ALTER TABLE IF EXISTS sports_kantin_products        FORCE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS sports_kantin_sale_items      FORCE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS sports_kantin_stock_movements FORCE ROW LEVEL SECURITY;
