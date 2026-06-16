-- Seed local para testes (sistema mãe de agendamento + canal de exemplo).
-- Login do painel (migration 000002): ferrariwill@gmail.com / 123456
-- API Key de teste: sk_live_local_test_key_0123456789abcdef0123456789ab

INSERT INTO systems (name, api_key_hash, webhook_url)
VALUES (
    'Sistema Local de Teste',
    '2a0f7fcee68e27731fc5c39bdb0a8c4b2ec7159bffab80efb0c5c624379da51a',
    'https://localhost/webhook'
)
ON CONFLICT (api_key_hash) DO NOTHING;

INSERT INTO client_channels (system_id, salon_name, external_client_id, phone_number_id, whatsapp_phone_number)
SELECT
    s.id,
    'Salão Demo',
    '45',
    'demo-phone-number-id',
    '5511999887766'
FROM systems s
WHERE s.api_key_hash = '2a0f7fcee68e27731fc5c39bdb0a8c4b2ec7159bffab80efb0c5c624379da51a'
ON CONFLICT (phone_number_id) DO NOTHING;
