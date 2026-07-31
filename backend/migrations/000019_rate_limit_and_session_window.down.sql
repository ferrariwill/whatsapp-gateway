DROP TABLE IF EXISTS outbound_retry_queue;
ALTER TABLE message_logs DROP COLUMN IF EXISTS failure_code;
DROP TABLE IF EXISTS rate_limit_audit;
ALTER TABLE systems
    DROP COLUMN IF EXISTS default_rate_limit_rps,
    DROP COLUMN IF EXISTS default_rate_limit_burst;
ALTER TABLE whatsapp_connections
    DROP COLUMN IF EXISTS rate_limit_rps,
    DROP COLUMN IF EXISTS rate_limit_burst;
DROP TABLE IF EXISTS contact_sessions;
