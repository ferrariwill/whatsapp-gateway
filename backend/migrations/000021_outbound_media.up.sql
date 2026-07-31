-- DEV-165: outbound media objects + retry kinds for image/document
-- Numbered 000021 because develop already has 000020_interactive_outbound_retry (DEV-166).
CREATE TABLE media_objects (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id     UUID NOT NULL REFERENCES systems(id) ON DELETE CASCADE,
    tenant_id     VARCHAR(255) NOT NULL,
    sha256        CHAR(64) NOT NULL,
    mime_type     VARCHAR(128) NOT NULL,
    byte_size     BIGINT NOT NULL,
    storage_path  TEXT NOT NULL,
    original_name TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ
);

CREATE INDEX idx_media_objects_tenant
    ON media_objects (system_id, tenant_id, created_at DESC);

CREATE INDEX idx_media_objects_expires
    ON media_objects (expires_at)
    WHERE expires_at IS NOT NULL;

ALTER TABLE outbound_retry_queue
    DROP CONSTRAINT IF EXISTS outbound_retry_queue_kind_check;

ALTER TABLE outbound_retry_queue
    ADD CONSTRAINT outbound_retry_queue_kind_check
    CHECK (kind IN ('template', 'text', 'plain_template', 'interactive', 'image', 'document'));
