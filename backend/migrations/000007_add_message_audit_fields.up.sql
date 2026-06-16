-- Campos de auditoria de conteúdo e direção das mensagens.

ALTER TABLE message_logs
    ADD COLUMN IF NOT EXISTS template_name VARCHAR(255),
    ADD COLUMN IF NOT EXISTS sent_content TEXT,
    ADD COLUMN IF NOT EXISTS received_content TEXT,
    ADD COLUMN IF NOT EXISTS direction VARCHAR(10) NOT NULL DEFAULT 'OUTBOUND';

CREATE INDEX IF NOT EXISTS idx_message_logs_audit_recent
    ON message_logs (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_message_logs_spam_window
    ON message_logs (system_id, external_client_id, created_at)
    WHERE direction = 'OUTBOUND';
