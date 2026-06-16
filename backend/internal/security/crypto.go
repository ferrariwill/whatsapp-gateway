package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const aes256KeySize = 32

// Encrypt criptografa plainText com AES-256-GCM e retorna o ciphertext em Base64.
// O nonce aleatório é prefixado ao ciphertext antes da codificação.
// secretKey deve ter exatamente 32 bytes (256 bits).
func Encrypt(plainText string, secretKey []byte) (string, error) {
	if len(secretKey) != aes256KeySize {
		return "", fmt.Errorf("secretKey must be %d bytes for AES-256", aes256KeySize)
	}

	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plainText), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decodifica cryptoText de Base64 e descriptografa com AES-256-GCM.
// secretKey deve ter exatamente 32 bytes (256 bits).
func Decrypt(cryptoText string, secretKey []byte) (string, error) {
	if len(secretKey) != aes256KeySize {
		return "", fmt.Errorf("secretKey must be %d bytes for AES-256", aes256KeySize)
	}

	raw, err := base64.StdEncoding.DecodeString(cryptoText)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plainText, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plainText), nil
}
