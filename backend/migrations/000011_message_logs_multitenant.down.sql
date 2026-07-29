DROP INDEX IF EXISTS idx_message_logs_sistema_origem;
DROP INDEX IF EXISTS idx_message_logs_connection_id;
ALTER TABLE message_logs DROP COLUMN IF EXISTS sistema_origem;
ALTER TABLE message_logs DROP COLUMN IF EXISTS connection_id;
