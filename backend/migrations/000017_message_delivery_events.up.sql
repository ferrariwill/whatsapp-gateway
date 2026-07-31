-- Histórico de status Meta (sent/delivered/read/failed) + estado do fan-out ao SaaS.
-- Separado de message_logs: uma mensagem pode ter vários status sem sobrescrever billing.
CREATE TABLE message_delivery_events (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id         UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    connection_id     UUID NOT NULL REFERENCES whatsapp_connections(id) ON DELETE CASCADE,
    tenant_id         TEXT NOT NULL,
    product_id        TEXT NOT NULL,
    meta_message_id   TEXT NOT NULL,
    recipient         TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL
        CHECK (status IN ('sent', 'delivered', 'read', 'failed')),
    meta_timestamp    TIMESTAMPTZ NOT NULL,
    errors_json       JSONB NOT NULL DEFAULT '[]',
    callback_status   TEXT NOT NULL DEFAULT 'pending'
        CHECK (callback_status IN ('pending', 'relaying', 'sent', 'failed', 'dlq')),
    callback_attempts INT NOT NULL DEFAULT 0,
    last_http_status  INT,
    last_error        TEXT,
    next_retry_at     TIMESTAMPTZ,
    sweep_claimed_at  TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_delivery_event_idempotency
        UNIQUE (meta_message_id, status, meta_timestamp)
);

CREATE INDEX idx_delivery_events_callback_pending
    ON message_delivery_events (next_retry_at, created_at)
    WHERE callback_status IN ('pending', 'failed');

CREATE INDEX idx_delivery_events_lookup
    ON message_delivery_events (system_id, meta_message_id, meta_timestamp);

CREATE INDEX idx_delivery_events_relaying
    ON message_delivery_events (sweep_claimed_at)
    WHERE callback_status = 'relaying';

-- Secret HMAC para assinar callbacks outbound ao produto (nunca logar).
ALTER TABLE whatsapp_connections
    ADD COLUMN IF NOT EXISTS webhook_secret TEXT;
