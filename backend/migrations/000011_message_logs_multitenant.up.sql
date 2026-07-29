ALTER TABLE message_logs ALTER COLUMN system_id DROP NOT NULL;
ALTER TABLE message_logs ADD COLUMN IF NOT EXISTS connection_id UUID REFERENCES whatsapp_connections (id) ON DELETE SET NULL;
ALTER TABLE message_logs ADD COLUMN IF NOT EXISTS sistema_origem VARCHAR(64);

CREATE INDEX IF NOT EXISTS idx_message_logs_connection_id ON message_logs (connection_id);
CREATE INDEX IF NOT EXISTS idx_message_logs_sistema_origem ON message_logs (sistema_origem);
