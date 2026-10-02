-- 039_kantin_soft_void.sql — voiding a sale must not erase it.
-- Idempotent.
--
-- WHY: voidSale hard-DELETED the sale row, cascading to its line items.
-- The stock was correctly restored and the movements recorded, so stock
-- stayed honest -- but the sale itself vanished. That is the wrong shape
-- for money: an owner who voids a 60k sale because the cashier rung up the
-- wrong item, then wants to know what happened to it, has nothing to look
-- at. Worse, a hard delete makes voiding idempotent-looking but not safe:
-- a retry after a partial failure restores stock a second time, and there
-- is no row left to notice it on. Voiding twice already did exactly that.
--
-- Soft void: the row stays, marked, with its line items intact. The
-- queries that count money filter on voided_at, so a voided sale stops
-- counting as revenue while remaining readable as history.
--
-- voided_by references sports_users so "who voided this" is answerable
-- without inventing a second staff table.

ALTER TABLE sports_kantin_sales
  ADD COLUMN IF NOT EXISTS voided_at timestamptz,
  ADD COLUMN IF NOT EXISTS voided_by uuid REFERENCES sports_users(id) ON DELETE SET NULL;

-- Every revenue query filters on this. A partial index keeps the index
-- small: only the rows that are still live need to be found.
CREATE INDEX IF NOT EXISTS sports_kantin_sales_live_idx
  ON sports_kantin_sales(tenant_id, created_at DESC)
  WHERE voided_at IS NULL;

-- Voiding is a financial correction, so it is part of the ledger, not an
-- afterthought: keep the id unique among live rows so a sale cannot be
-- voided twice by accident at the application layer.
CREATE UNIQUE INDEX IF NOT EXISTS sports_kantin_sales_live_id_idx
  ON sports_kantin_sales(id)
  WHERE voided_at IS NULL;