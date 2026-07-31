-- DEV-112: janela 24h (contact_sessions) + rate limit por tenant/produto + DLQ outbound.

CREATE TABLE contact_sessions (
    system_id       UUID         NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    tenant_id       VARCHAR(255) NOT NULL,
    wa_id           VARCHAR(32)  NOT NULL,
    last_inbound_at TIMESTAMPTZ  NOT NULL,
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (system_id, tenant_id, wa_id)
);
CREATE INDEX idx_contact_sessions_last_inbound ON contact_sessions (last_inbound_at);

ALTER TABLE whatsapp_connections
    ADD COLUMN IF NOT EXISTS rate_limit_rps   NUMERIC(10,3),
    ADD COLUMN IF NOT EXISTS rate_limit_burst INT;

ALTER TABLE systems
    ADD COLUMN IF NOT EXISTS default_rate_limit_rps   NUMERIC(10,3) NOT NULL DEFAULT 5,
    ADD COLUMN IF NOT EXISTS default_rate_limit_burst INT NOT NULL DEFAULT 10;

CREATE TABLE rate_limit_audit (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id       UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    tenant_id       VARCHAR(255),
    actor_user_id   UUID REFERENCES users(id),
    old_rps         NUMERIC(10,3),
    old_burst       INT,
    new_rps         NUMERIC(10,3),
    new_burst       INT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE message_logs
    ADD COLUMN IF NOT EXISTS failure_code TEXT;

CREATE TABLE outbound_retry_queue (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_log_id  UUID NOT NULL REFERENCES message_logs(id) ON DELETE CASCADE,
    system_id       UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    connection_id   UUID NOT NULL REFERENCES whatsapp_connections(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL DEFAULT 'template'
        CHECK (kind IN ('template', 'text', 'plain_template')),
    payload_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error      TEXT,
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'relaying', 'sent', 'failed', 'exhausted')),
    sweep_claimed_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_outbound_retry_message_log UNIQUE (message_log_id)
);

CREATE INDEX idx_outbound_retry_due
    ON outbound_retry_queue (next_attempt_at, created_at)
    WHERE status IN ('pending', 'failed');
