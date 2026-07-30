DROP INDEX IF EXISTS idx_message_logs_relaying_inbound;

ALTER TABLE message_logs DROP COLUMN IF EXISTS sweep_claimed_at;

ALTER TABLE message_logs DROP CONSTRAINT IF EXISTS message_logs_status_check;
ALTER TABLE message_logs
    ADD CONSTRAINT message_logs_status_check
    CHECK (status IN ('pending', 'sent', 'delivered', 'failed'));
