-- 042_drop_telegram.sql — Telegram channel deleted; PWA delivery lands later.
--
-- Telegram ingress, bot management, vault, and delivery are gone from the
-- Go service. This migration removes the Telegram delivery coupling while
-- preserving reminder rows and the atomic claim protocol:
--   agent_reminders.bot_id      dropped (Telegram-only recipient)
--   telegram_bots               dropped (bot registry + encrypted tokens)
--   claim_due_reminders         redefined without the Telegram bot join
--
-- Manual pre-deploy step: unschedule the Supabase `reminder-tick` cron and
-- undeploy the `deliver-reminders` Edge Function. The deployed function
-- expects `token_enc` and Telegram delivery, which no longer exist.

-- Redefine the claim without Telegram: same pending/sending protocol,
-- no bot join, no token ciphertext.
CREATE OR REPLACE FUNCTION private.claim_due_reminders(p_limit integer DEFAULT 20)
RETURNS SETOF jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_ids  uuid[];
  v_ts   timestamptz := pg_catalog.clock_timestamp();
BEGIN
  SELECT coalesce(pg_catalog.array_agg(c.id), '{}'::uuid[])
    INTO v_ids
  FROM (
    SELECT r.id
    FROM public.agent_reminders r
    WHERE r.status = 'pending'
      AND r.remind_at <= pg_catalog.now()
    ORDER BY r.remind_at
    LIMIT GREATEST(p_limit, 0)
    FOR UPDATE SKIP LOCKED
  ) c;

  IF v_ids = '{}'::uuid[] THEN
    RETURN;
  END IF;

  RETURN QUERY
  WITH claimed AS (
    UPDATE public.agent_reminders r
       SET status = 'sending',
           claimed_at = v_ts
     WHERE r.id = ANY(v_ids)
    RETURNING r.id, r.tenant_id, r.chat_id, r.text, r.attempts
  )
  SELECT jsonb_build_object(
    'id',         c.id,
    'tenant_id',  c.tenant_id,
    'chat_id',    c.chat_id,
    'text',       c.text,
    'attempts',   c.attempts,
    'claimed_at', v_ts
  )
  FROM claimed c;
END;
$$;

ALTER FUNCTION private.claim_due_reminders(integer) OWNER TO delivery_worker;

-- Drop the Telegram recipient column after the claim no longer reads it.
ALTER TABLE public.agent_reminders
  DROP CONSTRAINT IF EXISTS agent_reminders_bot_id_fkey;
ALTER TABLE public.agent_reminders
  DROP COLUMN IF EXISTS bot_id;

-- Revoke the Telegram table grant before dropping the table.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = 'public' AND table_name = 'telegram_bots'
  ) THEN
    EXECUTE 'REVOKE SELECT ON public.telegram_bots FROM delivery_worker';
  END IF;
END;
$$;

DROP TABLE IF EXISTS public.telegram_bots;
