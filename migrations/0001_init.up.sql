-- WasaTN initial schema.
--
-- Notes:
--   * gen_random_uuid() is built into PostgreSQL 13+, so no extension is required.
--   * Enums are used instead of free-form text to keep the domain strictly typed
--     (PROMPT.md: "no interface{} in core logic").
--   * E.164 assertions live on phone columns so bad numbers can never be stored,
--     even if the Go validation layer is bypassed.

CREATE TYPE user_plan AS ENUM ('free', 'starter', 'pro', 'business');

CREATE TYPE connection_status AS ENUM ('active', 'disconnected', 'token_expired');

CREATE TYPE template_category AS ENUM ('UTILITY', 'MARKETING', 'AUTHENTICATION');

CREATE TYPE template_status AS ENUM ('draft', 'pending', 'approved', 'rejected', 'paused');

CREATE TYPE campaign_status AS ENUM ('draft', 'scheduled', 'sending', 'paused', 'completed', 'failed');

CREATE TYPE schedule_kind AS ENUM ('now', 'at', 'recurring');

CREATE TYPE message_status AS ENUM ('queued', 'sent', 'delivered', 'read', 'failed', 'skipped');

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL,
    password_hash text NOT NULL,
    plan          user_plan NOT NULL DEFAULT 'free',
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_normalised CHECK (email = lower(email))
);

CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE connections (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    messaging_account_id  text NOT NULL,
    -- TODO(META-DOC): waac_id is part of Meta's new account model. The field is
    -- currently unavailable until Phase 2, but stored (nullable) so the schema
    -- does not need to change when Meta exposes it.
    waac_id               text,
    phone_number_id       text NOT NULL,
    phone_number          text NOT NULL,
    display_name          text NOT NULL DEFAULT '',
    -- AES-256-GCM ciphertext, never plaintext. See internal/cryptox.
    access_token_encrypted bytea NOT NULL,
    token_expires_at      timestamptz NOT NULL,
    quality_rating        text NOT NULL DEFAULT 'UNKNOWN',
    messaging_limit       integer NOT NULL DEFAULT 0,
    status                connection_status NOT NULL DEFAULT 'active',
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT connections_user_messaging_account_key UNIQUE (user_id, messaging_account_id),
    CONSTRAINT connections_user_phone_number_id_key UNIQUE (user_id, phone_number_id),
    CONSTRAINT connections_phone_e164 CHECK (phone_number ~ '^\+[1-9][0-9]{7,14}$'),
    CONSTRAINT connections_messaging_limit_positive CHECK (messaging_limit >= 0)
);

CREATE INDEX connections_user_id_idx ON connections (user_id);

CREATE TABLE contacts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    phone      text NOT NULL,
    name       text NOT NULL DEFAULT '',
    opt_in     boolean NOT NULL DEFAULT false,
    opt_in_at  timestamptz,
    variables  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT contacts_user_phone_key UNIQUE (user_id, phone),
    CONSTRAINT contacts_phone_e164 CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    -- Opt-in gate: a contact can only count as opted in with a recorded timestamp.
    CONSTRAINT contacts_opt_in_timestamp CHECK ((opt_in AND opt_in_at IS NOT NULL) OR NOT opt_in)
);

CREATE INDEX contacts_user_id_idx ON contacts (user_id);

CREATE TABLE templates (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id    uuid NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    meta_template_id text,
    name             text NOT NULL,
    category         template_category NOT NULL DEFAULT 'UTILITY',
    language         text NOT NULL DEFAULT 'en_US',
    -- Meta component array: header/body/footer/buttons.
    components       jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Body variable indices extracted on save, e.g. '{1,2}'.
    variables        text[] NOT NULL DEFAULT '{}',
    status           template_status NOT NULL DEFAULT 'draft',
    rejection_reason text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT templates_connection_name_language_key UNIQUE (connection_id, name, language),
    CONSTRAINT templates_name_snake_case CHECK (name ~ '^[a-z0-9]+(_[a-z0-9]+)*$')
);

CREATE INDEX templates_connection_id_idx ON templates (connection_id);
CREATE INDEX templates_status_idx ON templates (status);

CREATE TABLE campaigns (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id        uuid NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    template_id          uuid NOT NULL REFERENCES templates (id) ON DELETE RESTRICT,
    name                 text NOT NULL,
    schedule_kind        schedule_kind NOT NULL DEFAULT 'now',
    -- { "run_at": "...", "cron": "...", "timezone": "..." } for at/recurring.
    schedule             jsonb NOT NULL DEFAULT '{}'::jsonb,
    status               campaign_status NOT NULL DEFAULT 'draft',
    -- River periodic job backing recurring campaigns; NULL otherwise.
    river_periodic_job_id text,
    total_count          integer NOT NULL DEFAULT 0,
    sent_count           integer NOT NULL DEFAULT 0,
    delivered_count      integer NOT NULL DEFAULT 0,
    read_count           integer NOT NULL DEFAULT 0,
    failed_count         integer NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT campaigns_name_not_blank CHECK (length(btrim(name)) > 0),
    CONSTRAINT campaigns_counts_non_negative CHECK (
        total_count >= 0
        AND sent_count >= 0
        AND delivered_count >= 0
        AND read_count >= 0
        AND failed_count >= 0
    )
);

CREATE INDEX campaigns_user_id_idx ON campaigns (user_id, created_at DESC);
CREATE INDEX campaigns_connection_id_idx ON campaigns (connection_id);
CREATE INDEX campaigns_status_idx ON campaigns (status);

CREATE TABLE message_logs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id   uuid NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id    uuid REFERENCES contacts (id) ON DELETE SET NULL,
    wamid         text,
    status        message_status NOT NULL DEFAULT 'queued',
    error_code    text,
    error_message text,
    -- Why a queued message was never sent (e.g. "no_opt_in", "connection_disconnected").
    skip_reason   text,
    queued_at     timestamptz NOT NULL DEFAULT now(),
    sent_at       timestamptz,
    delivered_at  timestamptz,
    read_at       timestamptz,
    failed_at     timestamptz
);

-- Webhook processing is idempotent: Meta may redeliver a status update, and each
-- wamid maps to exactly one message row.
CREATE UNIQUE INDEX message_logs_wamid_key ON message_logs (wamid) WHERE wamid IS NOT NULL;
CREATE INDEX message_logs_campaign_id_idx ON message_logs (campaign_id);
CREATE INDEX message_logs_campaign_status_idx ON message_logs (campaign_id, status);

CREATE TABLE audit_logs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid REFERENCES users (id) ON DELETE SET NULL,
    action     text NOT NULL,
    entity     text NOT NULL,
    entity_id  text,
    meta       jsonb NOT NULL DEFAULT '{}'::jsonb,
    ip         inet,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_user_id_created_at_idx ON audit_logs (user_id, created_at DESC);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity, entity_id);