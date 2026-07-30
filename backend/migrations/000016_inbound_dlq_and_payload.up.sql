-- Estado de reprocessamento (DLQ) do inbound + payload tipado para replay fiel.
ALTER TABLE message_logs
    ADD COLUMN IF NOT EXISTS relay_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS failure_reason VARCHAR(32),
    ADD COLUMN IF NOT EXISTS last_error TEXT,
    ADD COLUMN IF NOT EXISTS inbound_payload JSONB;

-- Tentativas outbound recusadas pelo rate limiter são auditadas, mas não
-- representam envio aceito e portanto usam status próprio.
ALTER TABLE message_logs DROP CONSTRAINT IF EXISTS message_logs_status_check;
ALTER TABLE message_logs
    ADD CONSTRAINT message_logs_status_check
    CHECK (status IN ('pending', 'relaying', 'sent', 'delivered', 'failed', 'rejected'));

CREATE INDEX IF NOT EXISTS idx_message_logs_failed_inbound_retry
    ON message_logs (next_attempt_at NULLS FIRST, created_at)
    WHERE direction = 'INBOUND'
      AND status = 'failed'
      AND COALESCE(failure_reason, '') NOT IN ('permanent', 'exhausted');
