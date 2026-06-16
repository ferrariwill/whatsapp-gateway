ALTER TABLE client_channels DROP COLUMN IF EXISTS salon_name;
ALTER TABLE client_channels DROP COLUMN IF EXISTS phone_number_id;

ALTER TABLE client_channels ADD COLUMN IF NOT EXISTS phone_number_id_hash CHAR(64);
ALTER TABLE client_channels ADD COLUMN IF NOT EXISTS encrypted_phone_number_id TEXT;

ALTER TABLE systems ADD COLUMN IF NOT EXISTS encrypted_meta_token TEXT NOT NULL DEFAULT '';
ALTER TABLE systems ADD COLUMN IF NOT EXISTS encrypted_phone_number_id TEXT NOT NULL DEFAULT '';
ALTER TABLE systems ADD COLUMN IF NOT EXISTS encrypted_waba_id TEXT NOT NULL DEFAULT '';
