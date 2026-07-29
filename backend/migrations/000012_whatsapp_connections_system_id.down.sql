DROP INDEX IF EXISTS idx_whatsapp_connections_system_id;

ALTER TABLE whatsapp_connections DROP CONSTRAINT IF EXISTS uq_whatsapp_connections_system_tenant;
ALTER TABLE whatsapp_connections ADD CONSTRAINT uq_whatsapp_connections_origem_tenant UNIQUE (sistema_origem, tenant_id);

ALTER TABLE whatsapp_connections DROP COLUMN IF EXISTS system_id;

DROP INDEX IF EXISTS idx_systems_slug;
ALTER TABLE systems DROP COLUMN IF EXISTS slug;
