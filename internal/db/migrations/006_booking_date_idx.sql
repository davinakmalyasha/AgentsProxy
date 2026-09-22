-- 006_booking_date_idx.sql — Phase 3: index for the booking feature.
-- DONE: PRD 6.5 (feature platform) / STANDARDS A, E

CREATE INDEX IF NOT EXISTS tenant_records_booking_date_idx
  ON tenant_records ((payload->>'date'))
  WHERE kind = 'booking';
