ALTER TABLE outbound_retry_queue
    DROP CONSTRAINT IF EXISTS outbound_retry_queue_kind_check;

ALTER TABLE outbound_retry_queue
    ADD CONSTRAINT outbound_retry_queue_kind_check
    CHECK (kind IN ('template', 'text', 'plain_template', 'interactive'));

DROP TABLE IF EXISTS media_objects;
