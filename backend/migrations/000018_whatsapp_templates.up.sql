-- Catálogo local de templates WhatsApp (DEV-110).
CREATE TABLE whatsapp_templates (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id             UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    tenant_id             VARCHAR(255) NOT NULL,
    connection_id         UUID REFERENCES whatsapp_connections(id) ON DELETE SET NULL,
    waba_id               VARCHAR(64) NOT NULL,
    meta_id               VARCHAR(64),
    name                  VARCHAR(512) NOT NULL,
    language              VARCHAR(16) NOT NULL,
    category              VARCHAR(64),
    status                VARCHAR(32) NOT NULL
        CHECK (status IN ('APPROVED','PENDING','REJECTED','PAUSED','DISABLED')),
    components_json       JSONB NOT NULL DEFAULT '[]'::jsonb,
    expected_body_params  INT NOT NULL DEFAULT 0,
    quality_score         VARCHAR(32),
    synced_at             TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_whatsapp_templates_tenant_name_lang
        UNIQUE (system_id, tenant_id, name, language)
);

CREATE INDEX idx_whatsapp_templates_system_tenant
    ON whatsapp_templates (system_id, tenant_id);
CREATE INDEX idx_whatsapp_templates_waba
    ON whatsapp_templates (waba_id);
CREATE INDEX idx_whatsapp_templates_status
    ON whatsapp_templates (system_id, tenant_id, status);
CREATE INDEX idx_whatsapp_templates_meta_id
    ON whatsapp_templates (meta_id)
    WHERE meta_id IS NOT NULL;

-- Auditoria de sync por conexão
ALTER TABLE whatsapp_connections
    ADD COLUMN IF NOT EXISTS templates_synced_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS templates_sync_error TEXT,
    ADD COLUMN IF NOT EXISTS templates_synced_by VARCHAR(64);

-- Dedupe de webhooks de gestão de template
CREATE TABLE webhook_event_dedupe (
    event_key   TEXT PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
