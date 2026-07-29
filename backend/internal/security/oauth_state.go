package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const embeddedSignupStatePrefix = "v1."

// SignEmbeddedSignupState produz um state OAuth assinado (HMAC-SHA256) com
// slug, tenant_id, expiração e nonce. Formato:
//
//	v1.{b64url(slug)}.{b64url(tenant_id)}.{expUnix}.{nonce}.{mac}
//
// O separador entre slug e tenant não é ambíguo — tenant_id pode conter "_".
func SignEmbeddedSignupState(secret, slug, tenantID string, ttl time.Duration) (string, error) {
	secret = strings.TrimSpace(secret)
	slug = strings.TrimSpace(strings.ToLower(slug))
	tenantID = strings.TrimSpace(tenantID)
	if secret == "" {
		return "", fmt.Errorf("oauth state secret is required")
	}
	if slug == "" || tenantID == "" {
		return "", fmt.Errorf("slug and tenant_id are required")
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}

	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("generate oauth state nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	exp := strconv.FormatInt(time.Now().UTC().Add(ttl).Unix(), 10)

	payload := encodeEmbeddedSignupPayload(slug, tenantID, exp, nonce)
	mac := signEmbeddedSignupPayload(secret, payload)
	return embeddedSignupStatePrefix + payload + "." + mac, nil
}

// VerifyEmbeddedSignupState valida e extrai slug/tenant de um state assinado.
// Retorna ok=false (sem erro) quando o state não usa o formato assinado.
func VerifyEmbeddedSignupState(secret, state string) (slug, tenantID string, ok bool, err error) {
	state = strings.TrimSpace(state)
	if !strings.HasPrefix(state, embeddedSignupStatePrefix) {
		return "", "", false, nil
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", "", false, fmt.Errorf("oauth state secret is required to verify signed state")
	}

	raw := strings.TrimPrefix(state, embeddedSignupStatePrefix)
	parts := strings.Split(raw, ".")
	if len(parts) != 5 {
		return "", "", false, fmt.Errorf("signed oauth state malformed")
	}

	payload := strings.Join(parts[:4], ".")
	providedMAC := parts[4]
	expectedMAC := signEmbeddedSignupPayload(secret, payload)
	if !hmac.Equal([]byte(providedMAC), []byte(expectedMAC)) {
		return "", "", false, fmt.Errorf("signed oauth state signature invalid")
	}

	slugBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", false, fmt.Errorf("signed oauth state slug encoding invalid")
	}
	tenantBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false, fmt.Errorf("signed oauth state tenant encoding invalid")
	}
	expUnix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", false, fmt.Errorf("signed oauth state expiry invalid")
	}
	if time.Now().UTC().Unix() > expUnix {
		return "", "", false, fmt.Errorf("signed oauth state expired")
	}

	slug = strings.TrimSpace(strings.ToLower(string(slugBytes)))
	tenantID = strings.TrimSpace(string(tenantBytes))
	if slug == "" || tenantID == "" {
		return "", "", false, fmt.Errorf("signed oauth state missing slug or tenant_id")
	}
	return slug, tenantID, true, nil
}

func encodeEmbeddedSignupPayload(slug, tenantID, exp, nonce string) string {
	return strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(slug)),
		base64.RawURLEncoding.EncodeToString([]byte(tenantID)),
		exp,
		nonce,
	}, ".")
}

func signEmbeddedSignupPayload(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
