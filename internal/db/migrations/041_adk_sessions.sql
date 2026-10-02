-- 041_adk_sessions.sql — conversation-session tables for the ADK runner.
--
-- ADK's database session service owns four tables: sessions, events,
-- app_states, user_states. The DDL below is transcribed from AutoMigrate
-- output (dumped 2026-10-02 against ADK v2.5.0), so what this file creates
-- is byte-for-byte what the library expects; VerifySessionTables fails
-- boot loudly if any of them is absent.
--
-- These are app-scoped infrastructure, not tenant rows: every query the
-- service issues is keyed by (app_name, user_id, session_id), and the app
-- name IS the tenant slug. There is no tenant_id column to write an
-- isolation policy against, exactly like clients/usage in 033.
--
-- RLS stays enabled (the platform default — see 033 for what RLS with no
-- policies does to a non-owner role), with one permissive policy per
-- table scoped TO app_runtime, the only non-owner role holding any
-- privilege here. RLS is NOT forced, so the owner keeps full access for
-- maintenance, same as the 033 tables.
--
-- All statements are idempotent (IF NOT EXISTS / DROP IF EXISTS first):
-- the file must apply both on a fresh database and on one where
-- AutoMigrate already created the tables.

CREATE TABLE IF NOT EXISTS public.sessions (
    app_name text NOT NULL,
    user_id text NOT NULL,
    id text NOT NULL,
    state jsonb,
    create_time timestamp(6) with time zone,
    update_time timestamp(6) with time zone
);

CREATE TABLE IF NOT EXISTS public.events (
    id text NOT NULL,
    app_name text NOT NULL,
    user_id text NOT NULL,
    session_id text NOT NULL,
    invocation_id text,
    author text,
    actions bytea,
    long_running_tool_ids_json jsonb,
    routes_json jsonb,
    output_json jsonb,
    node_info_json jsonb,
    requested_input_json jsonb,
    branch text,
    isolation_scope text,
    "timestamp" timestamp(6) with time zone,
    content jsonb,
    grounding_metadata jsonb,
    custom_metadata jsonb,
    usage_metadata jsonb,
    citation_metadata jsonb,
    input_transcription jsonb,
    output_transcription jsonb,
    partial boolean,
    turn_complete boolean,
    error_code text,
    error_message text,
    interrupted boolean
);

CREATE TABLE IF NOT EXISTS public.app_states (
    app_name text NOT NULL,
    state jsonb,
    update_time timestamp(6) with time zone
);

CREATE TABLE IF NOT EXISTS public.user_states (
    app_name text NOT NULL,
    user_id text NOT NULL,
    state jsonb,
    update_time timestamp(6) with time zone
);

-- Constraints are dropped and re-added (rather than created once) so the
-- file applies both fresh and where AutoMigrate already created the
-- tables. Order matters: the events FK references the sessions PK, so
-- the FK drops first and is re-added last.
ALTER TABLE ONLY public.events DROP CONSTRAINT IF EXISTS fk_sessions_events;

ALTER TABLE ONLY public.sessions DROP CONSTRAINT IF EXISTS sessions_pkey;
ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (app_name, user_id, id);

ALTER TABLE ONLY public.events DROP CONSTRAINT IF EXISTS events_pkey;
ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id, app_name, user_id, session_id);

ALTER TABLE ONLY public.app_states DROP CONSTRAINT IF EXISTS app_states_pkey;
ALTER TABLE ONLY public.app_states
    ADD CONSTRAINT app_states_pkey PRIMARY KEY (app_name);

ALTER TABLE ONLY public.user_states DROP CONSTRAINT IF EXISTS user_states_pkey;
ALTER TABLE ONLY public.user_states
    ADD CONSTRAINT user_states_pkey PRIMARY KEY (app_name, user_id);

ALTER TABLE ONLY public.events
    ADD CONSTRAINT fk_sessions_events FOREIGN KEY (app_name, user_id, session_id)
    REFERENCES public.sessions(app_name, user_id, id) ON DELETE CASCADE;

ALTER TABLE public.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.events ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.app_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.user_states ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sessions_runtime ON public.sessions;
CREATE POLICY sessions_runtime ON public.sessions
    FOR ALL
    TO app_runtime
    USING (true)
    WITH CHECK (true);

DROP POLICY IF EXISTS events_runtime ON public.events;
CREATE POLICY events_runtime ON public.events
    FOR ALL
    TO app_runtime
    USING (true)
    WITH CHECK (true);

DROP POLICY IF EXISTS app_states_runtime ON public.app_states;
CREATE POLICY app_states_runtime ON public.app_states
    FOR ALL
    TO app_runtime
    USING (true)
    WITH CHECK (true);

DROP POLICY IF EXISTS user_states_runtime ON public.user_states;
CREATE POLICY user_states_runtime ON public.user_states
    FOR ALL
    TO app_runtime
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON public.sessions TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.events TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.app_states TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.user_states TO app_runtime;
