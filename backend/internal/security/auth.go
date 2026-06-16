package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const (
	apiKeyPrefix     = "sk_live_"
	apiKeyRandomSize = 32
)

// GenerateAPIKey gera uma chave de API única para sistemas clientes.
// Formato: sk_live_ + 32 bytes aleatórios codificados em hexadecimal (64 caracteres).
func GenerateAPIKey() (string, error) {
	randomBytes := make([]byte, apiKeyRandomSize)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}

	return apiKeyPrefix + hex.EncodeToString(randomBytes), nil
}

// HashAPIKey calcula o hash SHA-256 da chave de API para armazenamento seguro no banco.
// Retorna o digest em hexadecimal (64 caracteres).
func HashAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// HashPhoneNumberID calcula o hash SHA-256 do phone_number_id da Meta para lookup indexado.
func HashPhoneNumberID(phoneNumberID string) string {
	return HashAPIKey(phoneNumberID)
}

// HashPassword gera um hash bcrypt para armazenamento seguro de senhas de usuários.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword compara a senha informada com o hash bcrypt persistido no Supabase.
func VerifyPassword(password, hash string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return fmt.Errorf("invalid password")
	}
	return nil
}
