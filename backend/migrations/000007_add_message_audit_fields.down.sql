DROP INDEX IF EXISTS idx_message_logs_spam_window;
DROP INDEX IF EXISTS idx_message_logs_audit_recent;

ALTER TABLE message_logs
    DROP COLUMN IF EXISTS direction,
    DROP COLUMN IF EXISTS received_content,
    DROP COLUMN IF EXISTS sent_content,
    DROP COLUMN IF EXISTS template_name;
