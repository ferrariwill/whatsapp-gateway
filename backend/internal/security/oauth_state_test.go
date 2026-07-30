package security

import (
	"strings"
	"testing"
	"time"
)

func TestSignAndParseEmbeddedSignupStateRoundTrip(t *testing.T) {
	secret := "unit-test-oauth-secret"
	state, claims, err := SignEmbeddedSignupStateClaims(secret, "Beleza_Web", "salao_1", time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if claims.Slug != "beleza_web" || claims.TenantID != "salao_1" {
		t.Fatalf("claims slug/tenant = %q/%q", claims.Slug, claims.TenantID)
	}
	if claims.NonceHash == "" || len(claims.NonceHash) != 64 {
		t.Fatalf("nonce hash = %q, want 64-char hex", claims.NonceHash)
	}
	if claims.NonceHash != HashOAuthStateNonce(claims.Nonce) {
		t.Fatal("NonceHash must equal HashOAuthStateNonce(Nonce)")
	}

	parsed, ok, err := ParseEmbeddedSignupState(secret, state)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	if parsed.Slug != claims.Slug || parsed.TenantID != claims.TenantID {
		t.Errorf("parsed identity = %q/%q, want %q/%q", parsed.Slug, parsed.TenantID, claims.Slug, claims.TenantID)
	}
	if parsed.NonceHash != claims.NonceHash {
		t.Errorf("parsed nonce hash mismatch")
	}

	slug, tenantID, ok, err := VerifyEmbeddedSignupState(secret, state)
	if err != nil || !ok || slug != "beleza_web" || tenantID != "salao_1" {
		t.Fatalf("VerifyEmbeddedSignupState compat: slug=%q tenant=%q ok=%v err=%v", slug, tenantID, ok, err)
	}
}

func TestParseEmbeddedSignupStateRejectsTamperAndExpiry(t *testing.T) {
	secret := "unit-test-oauth-secret"
	state, _, err := SignEmbeddedSignupStateClaims(secret, "beleza_web", "t1", time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// O nonce é aleatório a cada execução, então o MAC muda: trocar o último
	// caractere por um literal fixo era no-op quando o MAC já terminava nele.
	// Derivar o substituto do próprio caractere garante alteração real sempre.
	replacement := byte('a')
	if state[len(state)-1] == replacement {
		replacement = 'b'
	}
	tampered := state[:len(state)-1] + string(replacement)
	if _, _, err := ParseEmbeddedSignupState(secret, tampered); err == nil {
		t.Fatal("expected signature error for tampered state")
	}

	expired, _, err := SignEmbeddedSignupStateClaims(secret, "beleza_web", "t1", -time.Minute)
	if err != nil {
		t.Fatalf("sign expired: %v", err)
	}
	if _, _, err := ParseEmbeddedSignupState(secret, expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got %v", err)
	}

	if _, ok, err := ParseEmbeddedSignupState(secret, "beleza_web_t1"); err != nil || ok {
		t.Fatalf("unsigned format should return ok=false without error, got ok=%v err=%v", ok, err)
	}
}

func TestHashOAuthStateNonceStable(t *testing.T) {
	a := HashOAuthStateNonce("abc")
	b := HashOAuthStateNonce("abc")
	c := HashOAuthStateNonce("abd")
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("hash stability failed: %q %q %q", a, b, c)
	}
}
