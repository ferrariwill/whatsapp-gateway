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

// EmbeddedSignupStateClaims é o conteúdo validado de um state OAuth assinado.
type EmbeddedSignupStateClaims struct {
	Slug      string
	TenantID  string
	Nonce     string
	NonceHash string
	ExpiresAt time.Time
}

// HashOAuthStateNonce calcula o SHA-256 hex do nonce (único valor persistido).
func HashOAuthStateNonce(nonce string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(nonce)))
	return hex.EncodeToString(sum[:])
}

// SignEmbeddedSignupState produz um state OAuth assinado (HMAC-SHA256) com
// slug, tenant_id, expiração e nonce. Formato:
//
//	v1.{b64url(slug)}.{b64url(tenant_id)}.{expUnix}.{nonce}.{mac}
//
// O separador entre slug e tenant não é ambíguo — tenant_id pode conter "_".
func SignEmbeddedSignupState(secret, slug, tenantID string, ttl time.Duration) (string, error) {
	state, _, err := SignEmbeddedSignupStateClaims(secret, slug, tenantID, ttl)
	return state, err
}

// SignEmbeddedSignupStateClaims assina o state e devolve as claims (inclui
// NonceHash / ExpiresAt para registro single-use no banco).
func SignEmbeddedSignupStateClaims(secret, slug, tenantID string, ttl time.Duration) (string, EmbeddedSignupStateClaims, error) {
	var claims EmbeddedSignupStateClaims
	secret = strings.TrimSpace(secret)
	slug = strings.TrimSpace(strings.ToLower(slug))
	tenantID = strings.TrimSpace(tenantID)
	if secret == "" {
		return "", claims, fmt.Errorf("oauth state secret is required")
	}
	if slug == "" || tenantID == "" {
		return "", claims, fmt.Errorf("slug and tenant_id are required")
	}
	if ttl == 0 {
		ttl = 30 * time.Minute
	}

	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", claims, fmt.Errorf("generate oauth state nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	expiresAt := time.Now().UTC().Add(ttl).Truncate(time.Second)
	exp := strconv.FormatInt(expiresAt.Unix(), 10)

	payload := encodeEmbeddedSignupPayload(slug, tenantID, exp, nonce)
	mac := signEmbeddedSignupPayload(secret, payload)
	state := embeddedSignupStatePrefix + payload + "." + mac

	claims = EmbeddedSignupStateClaims{
		Slug:      slug,
		TenantID:  tenantID,
		Nonce:     nonce,
		NonceHash: HashOAuthStateNonce(nonce),
		ExpiresAt: expiresAt,
	}
	return state, claims, nil
}

// VerifyEmbeddedSignupState valida e extrai slug/tenant de um state assinado.
// Retorna ok=false (sem erro) quando o state não usa o formato assinado.
// Mantida por compatibilidade; preferir ParseEmbeddedSignupState quando o
// callback precisar do nonce hash para consumo single-use.
func VerifyEmbeddedSignupState(secret, state string) (slug, tenantID string, ok bool, err error) {
	claims, ok, err := ParseEmbeddedSignupState(secret, state)
	if err != nil || !ok {
		return "", "", ok, err
	}
	return claims.Slug, claims.TenantID, true, nil
}

// ParseEmbeddedSignupState valida a assinatura HMAC e devolve as claims
// (inclui NonceHash e ExpiresAt). ok=false sem erro = formato não assinado.
func ParseEmbeddedSignupState(secret, state string) (claims EmbeddedSignupStateClaims, ok bool, err error) {
	state = strings.TrimSpace(state)
	if !strings.HasPrefix(state, embeddedSignupStatePrefix) {
		return claims, false, nil
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return claims, false, fmt.Errorf("oauth state secret is required to verify signed state")
	}

	raw := strings.TrimPrefix(state, embeddedSignupStatePrefix)
	parts := strings.Split(raw, ".")
	if len(parts) != 5 {
		return claims, false, fmt.Errorf("signed oauth state malformed")
	}

	payload := strings.Join(parts[:4], ".")
	providedMAC := parts[4]
	expectedMAC := signEmbeddedSignupPayload(secret, payload)
	if !hmac.Equal([]byte(providedMAC), []byte(expectedMAC)) {
		return claims, false, fmt.Errorf("signed oauth state signature invalid")
	}

	slugBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, false, fmt.Errorf("signed oauth state slug encoding invalid")
	}
	tenantBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, false, fmt.Errorf("signed oauth state tenant encoding invalid")
	}
	expUnix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return claims, false, fmt.Errorf("signed oauth state expiry invalid")
	}
	expiresAt := time.Unix(expUnix, 0).UTC()
	if time.Now().UTC().Unix() > expUnix {
		return claims, false, fmt.Errorf("signed oauth state expired")
	}

	nonce := strings.TrimSpace(parts[3])
	if nonce == "" {
		return claims, false, fmt.Errorf("signed oauth state missing nonce")
	}

	slug := strings.TrimSpace(strings.ToLower(string(slugBytes)))
	tenantID := strings.TrimSpace(string(tenantBytes))
	if slug == "" || tenantID == "" {
		return claims, false, fmt.Errorf("signed oauth state missing slug or tenant_id")
	}

	claims = EmbeddedSignupStateClaims{
		Slug:      slug,
		TenantID:  tenantID,
		Nonce:     nonce,
		NonceHash: HashOAuthStateNonce(nonce),
		ExpiresAt: expiresAt,
	}
	return claims, true, nil
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
