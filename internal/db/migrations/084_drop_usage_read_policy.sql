-- 084_drop_usage_read_policy.sql — quota reads go through month_tokens.
--
-- 053 deliberately removed the blanket usage_read_runtime policy: any
-- app-pool SQL could otherwise SUM every tenant's ledger. 063 re-created it
-- while fixing ENABLE RLS, which silently undid that boundary. The runtime
-- still needs INSERT for metering; reads belong to private.month_tokens and
-- the admin pool.

DROP POLICY IF EXISTS usage_read_runtime ON public.usage;
