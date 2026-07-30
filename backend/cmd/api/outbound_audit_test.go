package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
)

// brokenRepo devolve um repositório sobre um *sql.DB já fechado: toda escrita
// falha, sem precisar de Postgres.
func brokenRepo(t *testing.T) *repository.PostgresRepository {
	t.Helper()
	db, err := sql.Open("pgx", "postgresql://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close placeholder db: %v", err)
	}
	return repository.NewPostgresRepository(db)
}

func suspendedConnection() *model.WhatsAppConnection {
	return &model.WhatsAppConnection{
		ID:            "11111111-1111-1111-1111-111111111111",
		SystemID:      "22222222-2222-2222-2222-222222222222",
		SistemaOrigem: "beleza_web",
		TenantID:      "salao-1",
		PhoneNumberID: "phone-beleza",
		AccessToken:   "token-beleza",
		Status:        model.ConnectionStatusSuspendedSpam,
	}
}

// TestRateLimitedResponseStays429WhenAuditFails fixa a política decidida no
// code review do DEV-64: falha ao persistir a auditoria não vira 500. O cliente
// está sendo barrado, e um 5xx faria indisponibilidade momentânea do banco
// virar erro do gateway — cliente com retry agressivo em 5xx amplificaria o
// volume que o rate limit existe para conter.
func TestRateLimitedResponseStays429WhenAuditFails(t *testing.T) {
	t.Setenv("ADMIN_PHONE_NUMBER", "")

	srv := &server{
		repo:        brokenRepo(t),
		rateLimiter: security.NewRateLimiter(100, time.Minute),
		metaClient:  &http.Client{Timeout: time.Second},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-template", nil)

	allowed := srv.enforceConnectionSpamProtection(rec, req, suspendedConnection(), outboundAttemptAudit{
		AppointmentID: "appt-1",
		PhoneNumber:   "5511900000000",
		TemplateName:  "lembrete",
	})

	if allowed {
		t.Fatal("suspended connection was allowed through spam protection")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 even with the audit write failing", rec.Code)
	}
	if got := qaDecodeError(t, rec); got == "failed to persist rejected attempt" {
		t.Fatalf("response still exposes the audit failure as a gateway error: %q", got)
	}
}

// TestRateLimitExceededResponseStays429WhenAuditFails cobre o segundo caminho:
// recusa vinda do contador do rate limiter, não do status da conexão.
func TestRateLimitExceededResponseStays429WhenAuditFails(t *testing.T) {
	t.Setenv("ADMIN_PHONE_NUMBER", "")

	srv := &server{
		repo:        brokenRepo(t),
		rateLimiter: security.NewRateLimiter(1, time.Minute),
		metaClient:  &http.Client{Timeout: time.Second},
	}

	conn := suspendedConnection()
	conn.Status = model.ConnectionStatusActive

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/send-template", nil)

	if allowed := srv.enforceConnectionSpamProtection(httptest.NewRecorder(), req, conn, outboundAttemptAudit{}); !allowed {
		t.Fatal("first attempt should be within the limit")
	}

	rec := httptest.NewRecorder()
	if allowed := srv.enforceConnectionSpamProtection(rec, req, conn, outboundAttemptAudit{}); allowed {
		t.Fatal("second attempt exceeded the limit and should be refused")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 even with the audit write failing", rec.Code)
	}
}
