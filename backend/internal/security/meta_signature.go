package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// VerifyMetaSignature valida o header X-Hub-Signature-256 (HMAC-SHA256 do corpo
// cru com o app secret da Meta). Retorna false se o secret estiver vazio, o
// header estiver ausente/malformado ou o MAC não bater (comparação constant-time).
func VerifyMetaSignature(appSecret string, body []byte, signatureHeader string) bool {
	appSecret = strings.TrimSpace(appSecret)
	if appSecret == "" {
		return false
	}

	signatureHeader = strings.TrimSpace(signatureHeader)
	const prefix = "sha256="
	if !strings.HasPrefix(strings.ToLower(signatureHeader), prefix) {
		return false
	}
	provided := strings.TrimSpace(signatureHeader[len(prefix):])
	if provided == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(strings.ToLower(provided)), []byte(expected))
}

// SignMetaPayload gera o valor completo do header X-Hub-Signature-256 para testes
// e stubs que precisam imitar a Meta.
func SignMetaPayload(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
