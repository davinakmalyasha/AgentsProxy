-- 0013_rls_initplan.sql — evaluate the tenant GUC once per query, not per row.
--
-- Same initplan fix as platform migration 094, for the project schema.
-- This file runs with search_path set to the project schema, so table names
-- stay unqualified like the migrations that created them.
--
-- Semantics are unchanged: same policy names, commands, and predicates —
-- only the evaluation shape changes from a per-row call to a scalar
-- subquery initplan. Unset GUC still yields NULL and denies.
--
-- Additive and idempotent: DROP IF EXISTS + CREATE, no table or data change.

DROP POLICY IF EXISTS sports_bookings_isolation ON sports_bookings;
CREATE POLICY sports_bookings_isolation ON sports_bookings
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_court_blocks_isolation ON sports_court_blocks;
CREATE POLICY sports_court_blocks_isolation ON sports_court_blocks
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_customers_isolation ON sports_customers;
CREATE POLICY sports_customers_isolation ON sports_customers
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_expenses_isolation ON sports_expenses;
CREATE POLICY sports_expenses_isolation ON sports_expenses
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_facilities_isolation ON sports_facilities;
CREATE POLICY sports_facilities_isolation ON sports_facilities
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_inventory_isolation ON sports_inventory;
CREATE POLICY sports_inventory_isolation ON sports_inventory
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_kantin_products_isolation ON sports_kantin_products;
CREATE POLICY sports_kantin_products_isolation ON sports_kantin_products
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_kantin_items_isolation ON sports_kantin_sale_items;
CREATE POLICY sports_kantin_items_isolation ON sports_kantin_sale_items
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_kantin_sales_isolation ON sports_kantin_sales;
CREATE POLICY sports_kantin_sales_isolation ON sports_kantin_sales
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_kantin_movements_isolation ON sports_kantin_stock_movements;
CREATE POLICY sports_kantin_movements_isolation ON sports_kantin_stock_movements
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_member_topups_isolation ON sports_member_topups;
CREATE POLICY sports_member_topups_isolation ON sports_member_topups
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_members_isolation ON sports_members;
CREATE POLICY sports_members_isolation ON sports_members
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_payments_isolation ON sports_payments;
CREATE POLICY sports_payments_isolation ON sports_payments
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_pricing_rules_isolation ON sports_pricing_rules;
CREATE POLICY sports_pricing_rules_isolation ON sports_pricing_rules
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_recurring_series_isolation ON sports_recurring_series;
CREATE POLICY sports_recurring_series_isolation ON sports_recurring_series
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_staff_payments_isolation ON sports_staff_payments;
CREATE POLICY sports_staff_payments_isolation ON sports_staff_payments
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_users_read ON sports_users;
CREATE POLICY sports_users_read ON sports_users
  FOR SELECT
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_users_write ON sports_users;
CREATE POLICY sports_users_write ON sports_users
  FOR INSERT
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_users_update ON sports_users;
CREATE POLICY sports_users_update ON sports_users
  FOR UPDATE
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS sports_users_delete ON sports_users;
CREATE POLICY sports_users_delete ON sports_users
  FOR DELETE
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)));

DROP POLICY IF EXISTS tenant_records_isolation ON tenant_records;
CREATE POLICY tenant_records_isolation ON tenant_records
  FOR ALL
  USING (tenant_id = (SELECT current_setting('app.tenant_id', true)))
  WITH CHECK (tenant_id = (SELECT current_setting('app.tenant_id', true)));
