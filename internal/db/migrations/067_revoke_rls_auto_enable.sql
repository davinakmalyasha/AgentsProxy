-- 067: revoke API-executable helper installed by the Supabase RLS linter.
-- public.rls_auto_enable() is SECURITY DEFINER and, like every function,
-- carried the default PUBLIC EXECUTE grant -- which PostgREST exposes as
-- /rest/v1/rpc/rls_auto_enable to anon and authenticated. It is an event
-- trigger helper, not client surface: the event trigger invokes it as the
-- owner, so revoking caller EXECUTE changes nothing for its real use.
--
-- The function only exists on hosted Supabase projects (the dashboard
-- linter installs it); a fresh local/CI database never has it. Guard so
-- the migration is a no-op there instead of failing the whole chain.
do $$
begin
  if exists (
    select 1 from pg_proc p join pg_namespace n on n.oid = p.pronamespace
    where n.nspname = 'public' and p.proname = 'rls_auto_enable'
  ) then
    execute 'revoke execute on function public.rls_auto_enable() from public';
    execute 'revoke execute on function public.rls_auto_enable() from anon';
    execute 'revoke execute on function public.rls_auto_enable() from authenticated';
  end if;
end
$$;
