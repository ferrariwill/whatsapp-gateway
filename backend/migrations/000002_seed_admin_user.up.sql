-- Seed: usuário administrador inicial (local + produção).
-- E-mail : ferrariwill@gmail.com
-- Senha  : 123456
--
-- Hash gerado via security.HashPassword (bcrypt, cost 10) em internal/security/auth.go.
-- VerifyPassword usa bcrypt.CompareHashAndPassword — NÃO use SHA-256 para senhas de users.

INSERT INTO users (email, password_hash)
VALUES (
    'ferrariwill@gmail.com',
    '$2a$10$tGNYgstjQXFm02BBmIwRPepj94I2MjBMMpksX8Ml4L06SV5mWYjS6'
)
ON CONFLICT (email) DO UPDATE
SET password_hash = EXCLUDED.password_hash;
