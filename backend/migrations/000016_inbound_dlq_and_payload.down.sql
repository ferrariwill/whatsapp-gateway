DROP INDEX IF EXISTS idx_message_logs_failed_inbound_retry;

ALTER TABLE message_logs DROP CONSTRAINT IF EXISTS message_logs_status_check;
-- A versão anterior não conhece o status rejected. Preserve as linhas de
-- auditoria como falhas antes de restaurar o constraint antigo.
UPDATE message_logs SET status = 'failed' WHERE status = 'rejected';
ALTER TABLE message_logs
    ADD CONSTRAINT message_logs_status_check
    CHECK (status IN ('pending', 'relaying', 'sent', 'delivered', 'failed'));

ALTER TABLE message_logs
    DROP COLUMN IF EXISTS inbound_payload,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS failure_reason,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS relay_attempts;
