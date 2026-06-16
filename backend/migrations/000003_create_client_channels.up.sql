CREATE TABLE client_channels (
    id                          UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id                   UUID          NOT NULL REFERENCES systems (id) ON DELETE CASCADE,
    external_client_id          VARCHAR(255)  NOT NULL,
    phone_number_id_hash        CHAR(64)      NOT NULL UNIQUE,
    encrypted_phone_number_id   TEXT          NOT NULL,
    whatsapp_phone_number       VARCHAR(32)   NOT NULL,
    created_at                  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (system_id, external_client_id)
);

CREATE INDEX idx_client_channels_system_id ON client_channels (system_id);
CREATE INDEX idx_client_channels_phone_number_id_hash ON client_channels (phone_number_id_hash);
