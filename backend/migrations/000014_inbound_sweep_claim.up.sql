-- Status transitório do claim do sweep: pending → relaying → sent|failed.
-- sweep_claimed_at permite recuperar claims órfãos (processo morto no meio do POST).
ALTER TABLE message_logs DROP CONSTRAINT IF EXISTS message_logs_status_check;
ALTER TABLE message_logs
    ADD CONSTRAINT message_logs_status_check
    CHECK (status IN ('pending', 'relaying', 'sent', 'delivered', 'failed'));

ALTER TABLE message_logs
    ADD COLUMN IF NOT EXISTS sweep_claimed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_message_logs_relaying_inbound
    ON message_logs (sweep_claimed_at)
    WHERE direction = 'INBOUND' AND status = 'relaying';
