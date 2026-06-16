-- Token Meta centralizado no servidor (META_GLOBAL_TOKEN). Remove credenciais por tenant.

ALTER TABLE systems DROP COLUMN IF EXISTS encrypted_meta_token;
ALTER TABLE systems DROP COLUMN IF EXISTS encrypted_phone_number_id;
ALTER TABLE systems DROP COLUMN IF EXISTS encrypted_waba_id;

ALTER TABLE client_channels ADD COLUMN IF NOT EXISTS salon_name VARCHAR(255);
ALTER TABLE client_channels ADD COLUMN IF NOT EXISTS phone_number_id VARCHAR(64);

UPDATE client_channels
SET phone_number_id = 'legacy-' || id::text
WHERE phone_number_id IS NULL;

ALTER TABLE client_channels DROP COLUMN IF EXISTS phone_number_id_hash;
ALTER TABLE client_channels DROP COLUMN IF EXISTS encrypted_phone_number_id;

ALTER TABLE client_channels ALTER COLUMN phone_number_id SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_channels_phone_number_id ON client_channels (phone_number_id);
