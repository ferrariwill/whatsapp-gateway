-- Remove coluna de cobrança por mensagem; foco em auditoria de volume.

ALTER TABLE message_logs DROP COLUMN IF EXISTS cost_charged;
