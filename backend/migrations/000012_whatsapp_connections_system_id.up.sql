ALTER TABLE systems ADD COLUMN IF NOT EXISTS slug VARCHAR(64);

UPDATE systems
SET slug = lower(trim(both '_' from regexp_replace(regexp_replace(trim(name), '[^a-zA-Z0-9]+', '_', 'g'), '_+', '_', 'g')))
WHERE slug IS NULL OR slug = '';

UPDATE systems
SET slug = 'app_' || substr(replace(id::text, '-', ''), 1, 8)
WHERE slug IS NULL OR slug = '';

-- Desambigua slugs duplicados com sufixo do id.
UPDATE systems s
SET slug = left(s.slug, 55) || '_' || substr(replace(s.id::text, '-', ''), 1, 8)
WHERE EXISTS (
    SELECT 1 FROM systems s2
    WHERE s2.slug = s.slug AND s2.id <> s.id AND s2.created_at < s.created_at
);

ALTER TABLE systems ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_systems_slug ON systems (slug);

ALTER TABLE whatsapp_connections ADD COLUMN IF NOT EXISTS system_id UUID REFERENCES systems (id) ON DELETE CASCADE;

UPDATE whatsapp_connections wc
SET system_id = s.id
FROM systems s
WHERE wc.system_id IS NULL AND lower(wc.sistema_origem) = s.slug;

UPDATE whatsapp_connections wc
SET system_id = s.id
FROM systems s
WHERE wc.system_id IS NULL
  AND lower(wc.sistema_origem) = lower(trim(both '_' from regexp_replace(regexp_replace(trim(s.name), '[^a-zA-Z0-9]+', '_', 'g'), '_+', '_', 'g')));

UPDATE whatsapp_connections wc
SET system_id = (SELECT id FROM systems ORDER BY created_at ASC LIMIT 1)
WHERE wc.system_id IS NULL
  AND EXISTS (SELECT 1 FROM systems);

UPDATE whatsapp_connections wc
SET sistema_origem = s.slug
FROM systems s
WHERE wc.system_id = s.id;

ALTER TABLE whatsapp_connections ALTER COLUMN system_id SET NOT NULL;

ALTER TABLE whatsapp_connections DROP CONSTRAINT IF EXISTS uq_whatsapp_connections_origem_tenant;
ALTER TABLE whatsapp_connections ADD CONSTRAINT uq_whatsapp_connections_system_tenant UNIQUE (system_id, tenant_id);

CREATE INDEX IF NOT EXISTS idx_whatsapp_connections_system_id ON whatsapp_connections (system_id);
