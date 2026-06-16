CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Roles exigidas pelos GRANTs Supabase (ignoradas silenciosamente se já existirem).
DO $$ BEGIN CREATE ROLE authenticated NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE ROLE service_role NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE systems (
    id                          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name                        TEXT        NOT NULL,
    api_key_hash                CHAR(64)    NOT NULL UNIQUE,
    encrypted_meta_token        TEXT        NOT NULL,
    encrypted_phone_number_id   TEXT        NOT NULL,
    encrypted_waba_id           TEXT        NOT NULL,
    webhook_url                 TEXT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE users (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email           TEXT        NOT NULL UNIQUE,
    password_hash   TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE message_logs (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id       UUID        NOT NULL REFERENCES systems (id) ON DELETE CASCADE,
    appointment_id  TEXT        NOT NULL,
    phone_number    TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'delivered', 'failed')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_message_logs_system_id ON message_logs (system_id);
CREATE INDEX idx_message_logs_appointment_id ON message_logs (appointment_id);
CREATE INDEX idx_message_logs_status ON message_logs (status);

-- Permissões Supabase (roles ausentes em Postgres local são ignoradas).
REVOKE ALL ON TABLE public.systems FROM PUBLIC;
REVOKE ALL ON TABLE public.message_logs FROM PUBLIC;
REVOKE ALL ON TABLE public.users FROM PUBLIC;

GRANT USAGE ON SCHEMA public TO PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'postgres') THEN
        GRANT USAGE ON SCHEMA public TO postgres;
        GRANT ALL PRIVILEGES ON TABLE public.systems TO postgres;
        GRANT ALL PRIVILEGES ON TABLE public.message_logs TO postgres;
        GRANT ALL PRIVILEGES ON TABLE public.users TO postgres;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
        GRANT USAGE ON SCHEMA public TO authenticated;
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.systems TO authenticated;
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.message_logs TO authenticated;
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.users TO authenticated;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
        GRANT USAGE ON SCHEMA public TO service_role;
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.systems TO service_role;
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.message_logs TO service_role;
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.users TO service_role;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'postgres') THEN
        ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public
            GRANT ALL PRIVILEGES ON TABLES TO postgres;

        ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public
            GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO service_role, authenticated;
    END IF;
END $$;
