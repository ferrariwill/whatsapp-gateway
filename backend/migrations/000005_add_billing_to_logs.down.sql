DROP INDEX IF EXISTS idx_message_logs_meta_message_id;
DROP INDEX IF EXISTS idx_message_logs_external_client;
DROP INDEX IF EXISTS idx_message_logs_billing_period;

ALTER TABLE message_logs
    DROP COLUMN IF EXISTS delivered_at,
    DROP COLUMN IF EXISTS meta_cost,
    DROP COLUMN IF EXISTS cost_charged,
    DROP COLUMN IF EXISTS message_category,
    DROP COLUMN IF EXISTS meta_message_id,
    DROP COLUMN IF EXISTS external_client_id;
