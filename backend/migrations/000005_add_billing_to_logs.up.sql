-- Billing e auditoria de custos por mensagem WhatsApp.

ALTER TABLE message_logs
    ADD COLUMN IF NOT EXISTS external_client_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS meta_message_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS message_category VARCHAR(50),
    ADD COLUMN IF NOT EXISTS cost_charged NUMERIC(10, 4) NOT NULL DEFAULT 0.0000,
    ADD COLUMN IF NOT EXISTS meta_cost NUMERIC(10, 4) NOT NULL DEFAULT 0.0000,
    ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_message_logs_billing_period
    ON message_logs (system_id, created_at);

CREATE INDEX IF NOT EXISTS idx_message_logs_external_client
    ON message_logs (system_id, external_client_id, created_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_message_logs_meta_message_id
    ON message_logs (meta_message_id)
    WHERE meta_message_id IS NOT NULL;
