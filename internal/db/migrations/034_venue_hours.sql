-- 034_venue_hours.sql — one opening-hours setting instead of two that
-- disagreed. Idempotent.
--
-- WHY: venue hours lived in two tables with two different defaults.
-- sports_facilities defaulted to 06:00-22:00 (migration 007) and
-- sports_pricing_rules to 07:00-23:00 (migration 015). The booking form
-- offers its slots from the RULE, but the backend enforced the FACILITY,
-- so a 22:30 slot was priced successfully and then rejected — and no
-- single edit could fix it, because narrowing one table only moved the
-- mismatch into the other.
--
-- The owner-facing fix is one control (POST /v1/sports/pricing/apply_hours)
-- that writes both tables in one transaction. This migration only brings
-- existing rows onto the agreed value, and moves the COLUMN DEFAULTS so a
-- newly created court or rule starts consistent instead of reintroducing
-- the mismatch for the next tenant.
--
-- Only rows still sitting on the old default are touched. A tenant who
-- has deliberately set 06:00 or 22:00 keeps it, because that is now
-- indistinguishable from "never edited" and overwriting a real setting
-- would be worse than leaving a benign one.

-- Facilities still on the 007 default move to the agreed venue hours.
UPDATE sports_facilities
   SET open_time = '07:00', close_time = '23:00', updated_at = now()
 WHERE open_time = '06:00' AND close_time = '22:00';

-- Same for pricing rules still on the 015 default. This is a no-op
-- today, but it makes the intent explicit: if a tenant's rule was
-- narrowed from the default it is left alone, and the pair agrees.
UPDATE sports_pricing_rules
   SET open_time = '07:00', close_time = '23:00'
 WHERE open_time = '06:00' AND close_time = '22:00';

-- Column defaults, for rows created from here on.
ALTER TABLE sports_facilities      ALTER COLUMN open_time  SET DEFAULT '07:00';
ALTER TABLE sports_facilities      ALTER COLUMN close_time SET DEFAULT '23:00';
ALTER TABLE sports_pricing_rules   ALTER COLUMN open_time  SET DEFAULT '07:00';
ALTER TABLE sports_pricing_rules   ALTER COLUMN close_time SET DEFAULT '23:00';
