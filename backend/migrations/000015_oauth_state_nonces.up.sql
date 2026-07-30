-- Nonces single-use do state OAuth Embedded Signup.
-- Persiste somente o hash SHA-256 do nonce (nunca o valor em claro).
CREATE TABLE IF NOT EXISTS oauth_state_nonces (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    nonce_hash  CHAR(64)     NOT NULL,
    system_id   UUID         NOT NULL REFERENCES systems (id) ON DELETE CASCADE,
    tenant_id   VARCHAR(255) NOT NULL,
    expires_at  TIMESTAMPTZ  NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_oauth_state_nonces_hash UNIQUE (nonce_hash)
);

CREATE INDEX IF NOT EXISTS idx_oauth_state_nonces_active_expiry
    ON oauth_state_nonces (expires_at)
    WHERE consumed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_oauth_state_nonces_system_tenant
    ON oauth_state_nonces (system_id, tenant_id);
