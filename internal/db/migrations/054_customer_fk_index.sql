-- 054_customer_fk_index.sql — index the 023 customer_id FK.
--
-- The customers list/top visit-count join matches bookings to customers.
-- With the name/phone legacy path indexed (053) and the customer_id FK
-- unindexed, the FK leg would seq-scan. One btree fixes it.
--
-- DONE: perf pass / STANDARDS A, E

CREATE INDEX IF NOT EXISTS sports_bookings_customer_idx
  ON public.sports_bookings (customer_id);
