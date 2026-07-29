CREATE TABLE whatsapp_connections (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    sistema_origem      VARCHAR(64)  NOT NULL,
    tenant_id           VARCHAR(255) NOT NULL,
    waba_id             VARCHAR(64)  NOT NULL,
    phone_number_id     VARCHAR(64)  NOT NULL,
    access_token        TEXT         NOT NULL,
    webhook_url         TEXT,
    whatsapp_phone_number VARCHAR(32),
    status              VARCHAR(32)  NOT NULL DEFAULT 'ACTIVE',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_whatsapp_connections_origem_tenant UNIQUE (sistema_origem, tenant_id),
    CONSTRAINT uq_whatsapp_connections_phone_number_id UNIQUE (phone_number_id)
);

CREATE INDEX idx_whatsapp_connections_sistema_origem ON whatsapp_connections (sistema_origem);
CREATE INDEX idx_whatsapp_connections_tenant_id ON whatsapp_connections (tenant_id);
CREATE INDEX idx_whatsapp_connections_status ON whatsapp_connections (status);
