-- Índice parcial para o sweep de reconciliação do inbound: varre apenas as
-- linhas inbound que ficaram em pending, ordenadas por idade.
CREATE INDEX IF NOT EXISTS idx_message_logs_pending_inbound
    ON message_logs (created_at)
    WHERE direction = 'INBOUND' AND status = 'pending';
