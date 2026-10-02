-- 040_staff_phone.sql — let a staff member carry a phone number.
-- Idempotent.
--
-- WHY: the register request already accepted a `phone` field and the
-- handler bound it into the request struct -- and then dropped it, because
-- the INSERT did not name a column for it. There was no column to name.
-- So the field was accepted, validated by nobody, and silently discarded:
-- the owner filled in every new staff member's WhatsApp number and got
-- back a 201 with no phone on the record.
--
-- text, not varchar: Indonesian numbers arrive in whatever shape the owner
-- typed them (+62, 08, with spaces or dashes) and normalising them here
-- would be guessing. Stored as given, displayed as given.

ALTER TABLE sports_users
  ADD COLUMN IF NOT EXISTS phone text;

-- Staff are looked up by tenant and listed together; nothing filters on
-- phone, so no index -- a second index would only cost writes.
COMMENT ON COLUMN sports_users.phone IS
  'Staff contact number (WhatsApp). Free-form: stored as typed.';

-- private.login_user (migration 021) is the SECURITY DEFINER path login
-- uses, because sports_users is RLS-scoped and the login query runs with no
-- app.tenant_id set. It returns a fixed column list, so phone has to be
-- added there too or login -- and with it /auth/me -- would carry on
-- returning a user record with no phone in it, and the freshly registered
-- number would be invisible the moment anyone signed in.
--
-- OR REPLACE alone cannot do this: Postgres refuses to change a function's
-- return type in place. The DROP and CREATE have to run together, inside
-- the migration's transaction, so there is no window where either helper is
-- missing -- every request that calls them either sees both the old
-- definitions or both the new ones.
DROP FUNCTION IF EXISTS private.login_user(text, text);
CREATE FUNCTION private.login_user(p_tenant text, p_email text)
  RETURNS TABLE(user_id uuid, password_hash text, role text, full_name text, phone text, is_active boolean)
  LANGUAGE sql
  STABLE SECURITY DEFINER
  SET search_path TO ''
-- COALESCE, not a bare column: every pre-existing staff row has phone NULL,
-- and the Go side scans into a plain string, which pgx rejects with "cannot
-- scan NULL into *string". Without this, adding the column to the function
-- locks every existing owner out of their own app on their next sign-in.
AS $function$
  SELECT u.id, u.password_hash, u.role, u.full_name, COALESCE(u.phone, ''), u.is_active
    FROM public.sports_users u
   WHERE u.tenant_id = p_tenant AND u.email = p_email;
$function$;

-- private.session_user has the same fixed return list, and it backs
-- /auth/me and every authenticated request's identity. Left alone, a staff
-- member's phone would come back from register and then vanish on the next
-- sign-in.
DROP FUNCTION IF EXISTS private."session_user"(text);
CREATE FUNCTION private."session_user"(p_token_hash text)
  RETURNS TABLE(user_id uuid, tenant_id text, email text, role text, full_name text, phone text)
  LANGUAGE sql
  STABLE SECURITY DEFINER
  SET search_path TO ''
AS $function$
  SELECT u.id, u.tenant_id, u.email, u.role, u.full_name, COALESCE(u.phone, '')
    FROM public.sports_sessions s
    JOIN public.sports_users u ON u.id = s.user_id
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
     AND u.is_active
   LIMIT 1;
$function$;