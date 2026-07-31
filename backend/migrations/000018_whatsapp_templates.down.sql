DROP TABLE IF EXISTS webhook_event_dedupe;

ALTER TABLE whatsapp_connections
    DROP COLUMN IF EXISTS templates_synced_by,
    DROP COLUMN IF EXISTS templates_sync_error,
    DROP COLUMN IF EXISTS templates_synced_at;

DROP TABLE IF EXISTS whatsapp_templates;
