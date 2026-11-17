-- 071_drop_invite_columns.sql — invite-token portal entry is deleted.
--
-- The golden-standard login is email/username + password (superadmin adds
-- an email OTP step) and Google for clients. The invite-token magic link
-- (users.invite_token / invite_token_expiry, /auth/invite/verify,
-- /users/{id}/resend-invite, /client/invite page) is removed entirely:
-- admin-created users get a real password at creation, clients onboard
-- via Google or password. No code references these columns after this.
ALTER TABLE users DROP COLUMN IF EXISTS invite_token;
ALTER TABLE users DROP COLUMN IF EXISTS invite_token_expiry;
