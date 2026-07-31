-- Contadores diários de uso por product (system_id) + tenant (UTC).
CREATE TABLE usage_counters (
    system_id   UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    tenant_id   VARCHAR(255) NOT NULL,
    usage_date  DATE NOT NULL, -- UTC
    sent_text                   BIGINT NOT NULL DEFAULT 0,
    sent_template               BIGINT NOT NULL DEFAULT 0,
    sent_media                  BIGINT NOT NULL DEFAULT 0,
    inbound                     BIGINT NOT NULL DEFAULT 0,
    status_callback_delivered   BIGINT NOT NULL DEFAULT 0,
    errors_4xx                  BIGINT NOT NULL DEFAULT 0,
    errors_5xx                  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (system_id, tenant_id, usage_date)
);

CREATE INDEX idx_usage_counters_system_date
    ON usage_counters (system_id, usage_date);
CREATE INDEX idx_usage_counters_system_tenant_date
    ON usage_counters (system_id, tenant_id, usage_date);

-- Idempotência de status Meta: reentrega não re-incrementa contadores.
CREATE TABLE usage_status_dedup (
    system_id       UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    meta_message_id VARCHAR(255) NOT NULL,
    status          VARCHAR(32) NOT NULL, -- 'delivered' nesta fase
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (system_id, meta_message_id, status)
);
