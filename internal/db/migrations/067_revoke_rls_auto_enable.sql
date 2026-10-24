-- 067: revoke API-executable helper installed by the Supabase RLS linter.
-- public.rls_auto_enable() is SECURITY DEFINER and, like every function,
-- carried the default PUBLIC EXECUTE grant -- which PostgREST exposes as
-- /rest/v1/rpc/rls_auto_enable to anon and authenticated. It is an event
-- trigger helper, not client surface: the event trigger invokes it as the
-- owner, so revoking caller EXECUTE changes nothing for its real use.
revoke execute on function public.rls_auto_enable() from public;
revoke execute on function public.rls_auto_enable() from anon;
revoke execute on function public.rls_auto_enable() from authenticated;
