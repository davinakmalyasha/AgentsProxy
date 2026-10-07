-- 051_members_tombstones.sql — R1c: members joins the ledger discipline.
-- A member row (with balance) must never vanish by staff delete: tombstone
-- instead, hidden from lists/reads, preserved with who/when.
-- Additive only (GOVERNANCE §3): nullable columns, NULL = not deleted.
-- (Kantin products needs no columns: it already has is_active.)

ALTER TABLE sports_members
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
