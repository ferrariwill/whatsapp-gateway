-- Seed: aplicação mãe padrão para desenvolvimento local e painel admin.
-- API Key de teste: sk_live_local_test_key_0123456789abcdef0123456789ab

INSERT INTO systems (name, api_key_hash, webhook_url)
VALUES (
    'Sistema Local de Teste',
    '2a0f7fcee68e27731fc5c39bdb0a8c4b2ec7159bffab80efb0c5c624379da51a',
    'http://localhost:3000/webhook'
)
ON CONFLICT (api_key_hash) DO NOTHING;
