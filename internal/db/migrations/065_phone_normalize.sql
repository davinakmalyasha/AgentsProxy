-- 065_phone_normalize.sql — canonical Indonesian phone numbers.
--
-- `0812…`, `62812…`, `+62 812-…` and `812…` used to be four different
-- strings, so the same customer could exist twice and the legacy
-- bookings↔customers join (string equality on phone) missed rows.
-- Normalize at the STORAGE boundary with a BEFORE trigger: every writer
-- (HTTP, agent tools, scripts) gets one shape, and the existing
-- UNIQUE (tenant_id, phone) then also catches mixed-format duplicates.
--
-- Canonical: '+62' + digits for Indonesian shapes; other '+…' numbers
-- keep their country code; anything unrecognizable keeps its digits.
-- Pure function + triggers; the backfill only touches rows that change.
--
-- Collision guard: if two customers in one tenant would normalize to the
-- same number, the backfill raises with the offending tenant:number
-- instead of failing on the unique index mid-migration.

CREATE OR REPLACE FUNCTION private.normalize_phone(p text)
RETURNS text
LANGUAGE plpgsql
IMMUTABLE
SET search_path = ''
AS $$
DECLARE
  d text;
BEGIN
  IF p IS NULL THEN
    RETURN NULL;
  END IF;
  d := pg_catalog.regexp_replace(p, '[^0-9]', '', 'g');
  IF d = '' THEN
    RETURN p;
  END IF;
  -- Foreign number written with '+': keep the country code.
  IF left(btrim(p), 1) = '+' AND d NOT LIKE '62%' THEN
    RETURN '+' || d;
  END IF;
  IF d LIKE '62%' THEN
    RETURN '+62' || substr(d, 3);
  END IF;
  IF d LIKE '0%' THEN
    RETURN '+62' || substr(d, 2);
  END IF;
  IF d LIKE '8%' THEN
    RETURN '+62' || d;
  END IF;
  RETURN d;
END;
$$;
ALTER FUNCTION private.normalize_phone(text) OWNER TO postgres;
REVOKE ALL ON FUNCTION private.normalize_phone(text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION private.normalize_customer_phone_trg()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
  NEW.phone := private.normalize_phone(NEW.phone);
  RETURN NEW;
END;
$$;
ALTER FUNCTION private.normalize_customer_phone_trg() OWNER TO postgres;

CREATE OR REPLACE FUNCTION private.normalize_booking_phone_trg()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
  NEW.customer_phone := private.normalize_phone(NEW.customer_phone);
  RETURN NEW;
END;
$$;
ALTER FUNCTION private.normalize_booking_phone_trg() OWNER TO postgres;

DROP TRIGGER IF EXISTS sports_customers_phone_normalize ON public.sports_customers;
CREATE TRIGGER sports_customers_phone_normalize
  BEFORE INSERT OR UPDATE ON public.sports_customers
  FOR EACH ROW EXECUTE FUNCTION private.normalize_customer_phone_trg();

DROP TRIGGER IF EXISTS sports_bookings_phone_normalize ON public.sports_bookings;
CREATE TRIGGER sports_bookings_phone_normalize
  BEFORE INSERT OR UPDATE ON public.sports_bookings
  FOR EACH ROW EXECUTE FUNCTION private.normalize_booking_phone_trg();

-- Collision guard before the unique index can fire mid-backfill.
DO $$
DECLARE
  v_dup text;
BEGIN
  SELECT pg_catalog.string_agg(DISTINCT d.tenant_id || ':' || d.n, ', ')
    INTO v_dup
  FROM (
    SELECT tenant_id, private.normalize_phone(phone) AS n, count(*) AS c
      FROM public.sports_customers
     WHERE phone <> ''
     GROUP BY tenant_id, private.normalize_phone(phone)
    HAVING count(*) > 1
  ) d;
  IF v_dup IS NOT NULL THEN
    RAISE EXCEPTION 'phone normalization would collide customers: %', v_dup;
  END IF;
END $$;

UPDATE public.sports_customers
   SET phone = private.normalize_phone(phone)
 WHERE phone <> ''
   AND private.normalize_phone(phone) IS DISTINCT FROM phone;

UPDATE public.sports_bookings
   SET customer_phone = private.normalize_phone(customer_phone)
 WHERE customer_phone <> ''
   AND private.normalize_phone(customer_phone) IS DISTINCT FROM customer_phone;
