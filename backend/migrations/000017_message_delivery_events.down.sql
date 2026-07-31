DROP INDEX IF EXISTS idx_delivery_events_relaying;
DROP INDEX IF EXISTS idx_delivery_events_lookup;
DROP INDEX IF EXISTS idx_delivery_events_callback_pending;
DROP TABLE IF EXISTS message_delivery_events;

ALTER TABLE whatsapp_connections
    DROP COLUMN IF EXISTS webhook_secret;
