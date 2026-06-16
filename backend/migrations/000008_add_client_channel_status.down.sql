DROP INDEX IF EXISTS idx_client_channels_status;
ALTER TABLE client_channels DROP COLUMN IF EXISTS status;
