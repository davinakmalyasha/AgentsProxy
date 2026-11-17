-- 068_agency_init.sql — DFD agency plane tables (single-tenant ops).
-- Ported from DFDWebsite/api/db/migrations/000001_init.sql into this chain.
-- No RLS: DFD is the only tenant; cookie/bearer role checks guard access.

CREATE TABLE IF NOT EXISTS users (
    id                  BIGSERIAL PRIMARY KEY,
    username            TEXT NOT NULL UNIQUE,
    email               TEXT NOT NULL UNIQUE,
    password_hash       TEXT NOT NULL,
    role                TEXT NOT NULL DEFAULT 'EDITOR'
                        CHECK (role IN ('SUPERADMIN', 'EDITOR', 'CLIENT')),
    token_version       INTEGER NOT NULL DEFAULT 0,
    invite_token        TEXT UNIQUE,
    invite_token_expiry TIMESTAMPTZ,
    lead_id             BIGINT UNIQUE,
    deleted_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS leads (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL,
    whatsapp      TEXT NOT NULL,
    business_name TEXT,
    message       TEXT,
    status        TEXT NOT NULL DEFAULT 'NEW'
                  CHECK (status IN ('NEW', 'CONTACTED', 'CLOSED_WON', 'CLOSED_LOST')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_leads_whatsapp ON leads (whatsapp);
CREATE INDEX IF NOT EXISTS idx_leads_status ON leads (status);
CREATE INDEX IF NOT EXISTS idx_leads_deleted_at ON leads (deleted_at);

CREATE TABLE IF NOT EXISTS packages (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL,
    slug           TEXT NOT NULL UNIQUE,
    price          NUMERIC(12, 2) NOT NULL,
    discount_price NUMERIC(12, 2),
    features       JSONB NOT NULL DEFAULT '[]'::jsonb,
    is_active      BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order     INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_packages_active_sort ON packages (is_active, sort_order);
CREATE INDEX IF NOT EXISTS idx_packages_deleted_at ON packages (deleted_at);

CREATE TABLE IF NOT EXISTS promos (
    id         BIGSERIAL PRIMARY KEY,
    text       TEXT NOT NULL,
    link_url   TEXT,
    package_id BIGINT REFERENCES packages (id),
    is_active  BOOLEAN NOT NULL DEFAULT FALSE,
    start_date TIMESTAMPTZ,
    end_date   TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_promos_package ON promos (package_id);

CREATE TABLE IF NOT EXISTS orders (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lead_id           BIGINT NOT NULL REFERENCES leads (id),
    package_id        BIGINT NOT NULL REFERENCES packages (id),
    total_amount      NUMERIC(12, 2) NOT NULL,
    brief_data        JSONB NOT NULL DEFAULT '{}'::jsonb,
    status            TEXT NOT NULL DEFAULT 'PENDING_PAYMENT'
                      CHECK (status IN ('PENDING_PAYMENT', 'PROCESSING', 'REVISION', 'COMPLETED', 'CANCELLED')),
    agreed_to_terms_at TIMESTAMPTZ NOT NULL,
    current_step      INTEGER NOT NULL DEFAULT 0,
    drive_folder_id   TEXT,
    drive_folder_url  TEXT,
    idempotency_key   TEXT UNIQUE,
    handoff_url       TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_orders_lead ON orders (lead_id);
CREATE INDEX IF NOT EXISTS idx_orders_package ON orders (package_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders (status);
CREATE INDEX IF NOT EXISTS idx_orders_deleted_at ON orders (deleted_at);
CREATE INDEX IF NOT EXISTS idx_orders_created_at ON orders (created_at);

CREATE TABLE IF NOT EXISTS payments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id          UUID NOT NULL REFERENCES orders (id),
    payment_method    TEXT NOT NULL,
    payment_status    TEXT NOT NULL DEFAULT 'UNPAID'
                      CHECK (payment_status IN ('UNPAID', 'PAID', 'FAILED', 'REFUNDED')),
    payment_proof_url TEXT,
    paid_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payments_order ON payments (order_id);

CREATE TABLE IF NOT EXISTS order_notes (
    id         BIGSERIAL PRIMARY KEY,
    order_id   UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    content    TEXT NOT NULL,
    is_public  BOOLEAN NOT NULL DEFAULT FALSE,
    is_client  BOOLEAN NOT NULL DEFAULT FALSE,
    author_id  BIGINT REFERENCES users (id),
    is_edited  BOOLEAN NOT NULL DEFAULT FALSE,
    is_deleted BOOLEAN NOT NULL DEFAULT FALSE,
    parent_id  BIGINT REFERENCES order_notes (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_order_notes_order_deleted ON order_notes (order_id, is_deleted);
CREATE INDEX IF NOT EXISTS idx_order_notes_order_public ON order_notes (order_id, is_public);
CREATE INDEX IF NOT EXISTS idx_order_notes_author ON order_notes (author_id);
CREATE INDEX IF NOT EXISTS idx_order_notes_parent ON order_notes (parent_id);

CREATE TABLE IF NOT EXISTS order_resources (
    id         BIGSERIAL PRIMARY KEY,
    order_id   UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    title      TEXT NOT NULL,
    url        TEXT NOT NULL,
    icon       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_order_resources_order ON order_resources (order_id);

CREATE TABLE IF NOT EXISTS projects (
    id                 BIGSERIAL PRIMARY KEY,
    order_id           UUID UNIQUE REFERENCES orders (id),
    title              TEXT NOT NULL,
    slug               TEXT NOT NULL UNIQUE,
    client_name        TEXT NOT NULL,
    category           TEXT NOT NULL
                       CHECK (category IN ('PLATFORM', 'E_COMMERCE', 'LANDING', 'SAAS', 'CUSTOM', 'SERVICES', 'FNB', 'RETAIL', 'CORPORATE')),
    thumbnail_url      TEXT,
    description        TEXT NOT NULL,
    tech_stack         JSONB NOT NULL DEFAULT '[]'::jsonb,
    duration           TEXT NOT NULL,
    testimonial_quote  TEXT,
    testimonial_author TEXT,
    maintenance_status TEXT NOT NULL DEFAULT 'CLIENT_MANAGED'
                       CHECK (maintenance_status IN ('ACTIVE', 'INACTIVE', 'CLIENT_MANAGED')),
    domain_expiry_date TIMESTAMPTZ,
    seo_title          TEXT,
    seo_description    TEXT,
    views_count        INTEGER NOT NULL DEFAULT 0,
    is_featured        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_projects_category ON projects (category);
CREATE INDEX IF NOT EXISTS idx_projects_deleted_at ON projects (deleted_at);

CREATE TABLE IF NOT EXISTS project_images (
    id         BIGSERIAL PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    image_url  TEXT NOT NULL,
    caption    TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_project_images_project ON project_images (project_id);

CREATE TABLE IF NOT EXISTS hosting (
    id                BIGSERIAL PRIMARY KEY,
    domain            TEXT NOT NULL UNIQUE,
    client_name       TEXT NOT NULL,
    client_whatsapp   TEXT NOT NULL,
    client_email      TEXT,
    hosting_provider  TEXT NOT NULL,
    hosting_start_date TIMESTAMPTZ NOT NULL,
    hosting_end_date  TIMESTAMPTZ NOT NULL,
    domain_end_date   TIMESTAMPTZ,
    notify_before_days INTEGER NOT NULL DEFAULT 30,
    status            TEXT NOT NULL DEFAULT 'ACTIVE'
                      CHECK (status IN ('ACTIVE', 'EXPIRING_SOON', 'EXPIRED', 'CANCELLED')),
    notes             TEXT,
    project_id        BIGINT UNIQUE REFERENCES projects (id),
    last_notified_at  TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_hosting_status ON hosting (status);
CREATE INDEX IF NOT EXISTS idx_hosting_end_date ON hosting (hosting_end_date);

CREATE TABLE IF NOT EXISTS articles (
    id              BIGSERIAL PRIMARY KEY,
    title           TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,
    content         TEXT NOT NULL,
    thumbnail_url   TEXT,
    author_id       BIGINT REFERENCES users (id),
    is_published    BOOLEAN NOT NULL DEFAULT FALSE,
    views           INTEGER NOT NULL DEFAULT 0,
    tags            JSONB,
    seo_title       TEXT,
    seo_description TEXT,
    published_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_articles_author ON articles (author_id);
CREATE INDEX IF NOT EXISTS idx_articles_published ON articles (is_published);
CREATE INDEX IF NOT EXISTS idx_articles_published_at ON articles (published_at);

CREATE TABLE IF NOT EXISTS contracts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id   UUID NOT NULL UNIQUE REFERENCES orders (id),
    user_id    BIGINT REFERENCES users (id) ON DELETE SET NULL,
    pdf_url    TEXT,
    status     TEXT NOT NULL DEFAULT 'DRAFT',
    signed_at  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_contracts_user ON contracts (user_id);

CREATE TABLE IF NOT EXISTS order_milestones (
    id           BIGSERIAL PRIMARY KEY,
    order_id     UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    description  TEXT,
    cover_url    TEXT,
    category     TEXT NOT NULL DEFAULT 'OTHER'
                 CHECK (category IN ('DESIGN', 'FRONTEND', 'BACKEND', 'COPY', 'CONTENT', 'REVIEW', 'ASSET', 'OTHER')),
    status       TEXT NOT NULL DEFAULT 'READY'
                 CHECK (status IN ('DRAFT', 'READY', 'IN_REVIEW', 'REVISION_NEEDED', 'APPROVED', 'REJECTED')),
    notify_client BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_id BIGINT REFERENCES users (id),
    viewed_at    TIMESTAMPTZ,
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_milestones_order ON order_milestones (order_id);
CREATE INDEX IF NOT EXISTS idx_milestones_status ON order_milestones (status);
CREATE INDEX IF NOT EXISTS idx_milestones_created_at ON order_milestones (created_at);

CREATE TABLE IF NOT EXISTS milestone_views (
    id           BIGSERIAL PRIMARY KEY,
    milestone_id BIGINT NOT NULL REFERENCES order_milestones (id) ON DELETE CASCADE,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    viewed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (milestone_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_milestone_views_milestone ON milestone_views (milestone_id);

CREATE TABLE IF NOT EXISTS order_activities (
    id         BIGSERIAL PRIMARY KEY,
    order_id   UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    actor_id   BIGINT REFERENCES users (id) ON DELETE SET NULL,
    type       TEXT NOT NULL,
    message    TEXT NOT NULL,
    meta       JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_order_activities_order ON order_activities (order_id);
CREATE INDEX IF NOT EXISTS idx_order_activities_created_at ON order_activities (created_at);

CREATE TABLE IF NOT EXISTS client_requests (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT REFERENCES users (id),
    order_id         UUID REFERENCES orders (id),
    type             TEXT NOT NULL DEFAULT 'GENERAL'
                     CHECK (type IN ('ACCESS', 'FILE', 'MEETING', 'REVISION', 'GENERAL')),
    title            TEXT NOT NULL,
    body             TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'PENDING'
                     CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED')),
    admin_note       TEXT,
    drive_url        TEXT,
    calendly_event_url TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_client_requests_user_deleted ON client_requests (user_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_client_requests_order ON client_requests (order_id);
CREATE INDEX IF NOT EXISTS idx_client_requests_status_deleted ON client_requests (status, deleted_at);
CREATE INDEX IF NOT EXISTS idx_client_requests_created_at ON client_requests (created_at);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT REFERENCES users (id),
    actor_type TEXT NOT NULL DEFAULT 'USER',
    actor_id   TEXT,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL,
    details    JSONB,
    ip_address TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_user ON audit_logs (user_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs (action);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs (created_at);

CREATE TABLE IF NOT EXISTS global_settings (
    id                  BIGSERIAL PRIMARY KEY,
    whatsapp_number     TEXT NOT NULL,
    email_contact       TEXT NOT NULL,
    office_address      TEXT NOT NULL,
    instagram_link      TEXT,
    is_maintenance_mode BOOLEAN NOT NULL DEFAULT FALSE,
    meta_pixel_id       TEXT,
    google_analytics_id TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Machine-facing layer: API keys, outbox events, webhooks, idempotency.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    scopes       TEXT[] NOT NULL DEFAULT '{}'::text[],
    created_by   BIGINT REFERENCES users (id) ON DELETE SET NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys (key_prefix);

CREATE TABLE IF NOT EXISTS agent_events (
    id             BIGSERIAL PRIMARY KEY,
    event_type     TEXT NOT NULL,
    aggregate_type TEXT NOT NULL,
    aggregate_id   TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT,
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    processed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_events_pending ON agent_events (processed_at, id);
CREATE INDEX IF NOT EXISTS idx_agent_events_aggregate ON agent_events (aggregate_type, aggregate_id);

CREATE TABLE IF NOT EXISTS webhook_subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    event_types TEXT[] NOT NULL DEFAULT '{}',
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_by  BIGINT REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id            BIGSERIAL PRIMARY KEY,
    subscription_id UUID NOT NULL REFERENCES webhook_subscriptions (id) ON DELETE CASCADE,
    event_id      BIGINT NOT NULL REFERENCES agent_events (id) ON DELETE CASCADE,
    url           TEXT NOT NULL,
    status_code   INTEGER,
    response_body TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0,
    delivered_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event ON webhook_deliveries (event_id);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    idempotency_key TEXT PRIMARY KEY,
    request_hash  TEXT NOT NULL,
    response_body JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- updated_at maintenance.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'users', 'leads', 'packages', 'promos', 'orders', 'payments',
        'order_notes', 'order_resources', 'projects', 'project_images',
        'hosting', 'articles', 'contracts', 'order_milestones',
        'client_requests', 'global_settings'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_trigger
            WHERE tgname = t || '_set_updated_at'
              AND tgrelid = t::regclass
        ) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION set_updated_at()',
                t || '_set_updated_at', t
            );
        END IF;
    END LOOP;
END $$;

-- Agency tables are single-tenant (DFD ops). Access is guarded by cookie/
-- bearer role checks in handlers; the runtime pool needs table + sequence
-- rights (handlers run on the app pool).
GRANT SELECT, INSERT, UPDATE, DELETE ON
  users, leads, packages, promos, orders, payments,
  order_notes, order_resources, projects, project_images,
  hosting, articles, contracts, order_milestones, milestone_views,
  order_activities, client_requests, audit_logs, global_settings,
  api_keys, agent_events, webhook_subscriptions, webhook_deliveries,
  idempotency_keys
TO app_runtime;

GRANT USAGE, SELECT ON
  users_id_seq, leads_id_seq, packages_id_seq, promos_id_seq,
  order_notes_id_seq, order_resources_id_seq, projects_id_seq,
  project_images_id_seq, hosting_id_seq, articles_id_seq,
  order_milestones_id_seq, milestone_views_id_seq, order_activities_id_seq,
  client_requests_id_seq, audit_logs_id_seq, global_settings_id_seq,
  agent_events_id_seq, webhook_deliveries_id_seq
TO app_runtime;
