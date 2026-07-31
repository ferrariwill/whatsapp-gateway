package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginErrorMessageSession(t *testing.T) {
	t.Parallel()

	if got := loginErrorMessage("session"); got != "Sua sessão expirou. Faça login novamente." {
		t.Fatalf("session message = %q", got)
	}
	if loginErrorMessage("invalid") == loginErrorMessage("session") {
		t.Fatal("session and invalid messages must differ")
	}
}

func TestJWTMiddlewareRedirectsSessionExpired(t *testing.T) {
	t.Parallel()

	srv := &server{jwtSecret: []byte("qa-jwt-secret-with-at-least-32-chars!!")}
	handler := srv.jwtMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("browser", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
		}
		if loc := rec.Header().Get("Location"); loc != loginSessionExpiredPath {
			t.Fatalf("Location = %q, want %q", loc, loginSessionExpiredPath)
		}
	})

	t.Run("htmx", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if loc := rec.Header().Get("HX-Redirect"); loc != loginSessionExpiredPath {
			t.Fatalf("HX-Redirect = %q, want %q", loc, loginSessionExpiredPath)
		}
	})
}

func TestWriteEmbeddedSignupResultHTMLTemplate(t *testing.T) {
	t.Parallel()

	srv := &server{templates: loadTemplates()}
	req := httptest.NewRequest(http.MethodGet, "/meta/embedded-signup/callback", nil)
	rec := httptest.NewRecorder()
	srv.writeEmbeddedSignupResult(rec, req, http.StatusOK, true, "tenant ativo", "detalhe")

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{"Abrir painel", "/dashboard", "tenant ativo", "WhatsApp Gateway"} {
		if !strings.Contains(body, want) {
			t.Fatalf("HTML missing %q; body=%s", want, body)
		}
	}
}
