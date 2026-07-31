package main

// Harness de QA para os testes de integração do gateway.
//
// Sobe um banco descartável (whatsapp_gateway_qa) no Postgres de desenvolvimento,
// aplica as migrations embutidas e monta um *server real apontando para ele.
// A Meta é substituída por um stub de transporte HTTP: requisições para
// graph.facebook.com são interceptadas, qualquer outro host (webhook do SaaS em
// httptest) segue pela rede local.
//
// Sem Postgres acessível todos os testes fazem Skip — nunca falso-positivo.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whatsappgetway/gateway/internal/migrate"
	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/security"
	"github.com/whatsappgetway/gateway/internal/service"
)

const (
	qaDefaultAdminDatabaseURL = "postgresql://gateway:gateway@localhost:5433/postgres?sslmode=disable"
	qaMetaHost                = "graph.facebook.com"
)

var (
	qaDatabaseName = fmt.Sprintf("whatsapp_gateway_qa_%d", os.Getpid())
	qaDB           *sql.DB
	qaSkipReason   string
)

func TestMain(m *testing.M) {
	if err := setupQADatabase(); err != nil {
		qaSkipReason = err.Error()
		log.Printf("QA: integration database unavailable, integration tests will be skipped: %v", err)
	}

	code := m.Run()

	if qaDB != nil {
		_ = qaDB.Close()
	}
	if err := dropQADatabase(); err != nil {
		log.Printf("QA: cleanup database %s: %v", qaDatabaseName, err)
	}
	os.Exit(code)
}

func setupQADatabase() error {
	adminURL := qaAdminDatabaseURL()

	testURL, err := replaceDatabaseName(adminURL, qaDatabaseName)
	if err != nil {
		return err
	}

	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		return fmt.Errorf("open admin connection: %w", err)
	}
	defer admin.Close()

	if err := withQATimeout(10*time.Second, func(ctx context.Context) error {
		if err := admin.PingContext(ctx); err != nil {
			return fmt.Errorf("ping %s: %w", redactURL(adminURL), err)
		}
		if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+qaDatabaseName+` WITH (FORCE)`); err != nil {
			return fmt.Errorf("drop test database: %w", err)
		}
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+qaDatabaseName); err != nil {
			return fmt.Errorf("create test database: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if _, err := migrate.Up(testURL); err != nil {
		return fmt.Errorf("migrate test database: %w", err)
	}

	db, err := sql.Open("pgx", testURL)
	if err != nil {
		return fmt.Errorf("open test database: %w", err)
	}
	db.SetMaxOpenConns(30)
	db.SetMaxIdleConns(10)
	if err := withQATimeout(10*time.Second, db.PingContext); err != nil {
		return fmt.Errorf("ping test database: %w", err)
	}

	qaDB = db
	return nil
}

func qaAdminDatabaseURL() string {
	if value := strings.TrimSpace(os.Getenv("QA_ADMIN_DATABASE_URL")); value != "" {
		return value
	}
	return qaDefaultAdminDatabaseURL
}

func dropQADatabase() error {
	admin, err := sql.Open("pgx", qaAdminDatabaseURL())
	if err != nil {
		return err
	}
	defer admin.Close()
	return withQATimeout(10*time.Second, func(ctx context.Context) error {
		_, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+qaDatabaseName+` WITH (FORCE)`)
		return err
	})
}

func withQATimeout(timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return fn(ctx)
}

func replaceDatabaseName(rawURL, dbName string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func redactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "database url"
	}
	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}
	return parsed.String()
}

// requireQADB pula o teste quando o Postgres de QA não está disponível.
func requireQADB(t *testing.T) {
	t.Helper()
	if qaDB == nil {
		t.Skipf("QA integration database unavailable: %s", qaSkipReason)
	}
	resetQAData(t)
}

func resetQAData(t *testing.T) {
	t.Helper()
	_, err := qaDB.Exec(`TRUNCATE message_delivery_events, oauth_state_nonces, message_logs, whatsapp_connections, client_channels, systems RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset qa data: %v", err)
	}
}

// ----------------------------------------------------------------------------
// Stub da Meta
// ----------------------------------------------------------------------------

// qaMetaCall registra uma chamada capturada para a Graph API.
type qaMetaCall struct {
	Method        string
	Path          string
	AccessToken   string
	PhoneNumberID string
	To            string
	TemplateName  string
	Body          []byte
}

// qaMetaStub intercepta a Graph API e deixa passar qualquer outro host.
type qaMetaStub struct {
	mu    sync.Mutex
	calls []qaMetaCall

	seq atomic.Int64

	// respond, quando definido, decide o status/corpo devolvido pela Meta.
	respond func(call qaMetaCall, seq int64) (int, string)
}

func newQAMetaStub() *qaMetaStub {
	return &qaMetaStub{}
}

func (s *qaMetaStub) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != qaMetaHost {
		return http.DefaultTransport.RoundTrip(r)
	}

	body := []byte{}
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}

	call := qaMetaCall{
		Method:        r.Method,
		Path:          r.URL.Path,
		AccessToken:   strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		PhoneNumberID: phoneNumberIDFromGraphPath(r.URL.Path),
		Body:          body,
	}

	var parsed struct {
		To       string `json:"to"`
		Template struct {
			Name string `json:"name"`
		} `json:"template"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		call.To = parsed.To
		call.TemplateName = parsed.Template.Name
	}

	seq := s.seq.Add(1)

	s.mu.Lock()
	s.calls = append(s.calls, call)
	respond := s.respond
	s.mu.Unlock()

	status, responseBody := http.StatusOK, fmt.Sprintf(`{"messages":[{"id":"wamid.QA.%06d"}]}`, seq)
	if respond != nil {
		status, responseBody = respond(call, seq)
	}

	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
		Request:    r,
	}, nil
}

func phoneNumberIDFromGraphPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

func (s *qaMetaStub) Calls() []qaMetaCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]qaMetaCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *qaMetaStub) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// ----------------------------------------------------------------------------
// Servidor sob teste
// ----------------------------------------------------------------------------

func newQAServer(t *testing.T, stub *qaMetaStub, maxMessagesPerMinute int) *server {
	t.Helper()
	return newQAServerWithRelay(t, stub, maxMessagesPerMinute, relayPoolConfig{
		Workers:       32,
		QueueSize:     512,
		SubmitTimeout: 2 * time.Second,
		TaskTimeout:   45 * time.Second,
	})
}

// newQAServerWithRelay monta o servidor com um pool de repasse próprio, drenado
// no fim do teste — os testes exercitam o mesmo caminho de admissão da produção.
func newQAServerWithRelay(
	t *testing.T,
	stub *qaMetaStub,
	maxMessagesPerMinute int,
	relayCfg relayPoolConfig,
) *server {
	t.Helper()
	t.Setenv("ADMIN_PHONE_NUMBER", "")
	t.Setenv("MOTHER_SYSTEM_WEBHOOK_URL", "")
	if strings.TrimSpace(os.Getenv("META_APP_SECRET")) == "" {
		t.Setenv("META_APP_SECRET", "qa-app-secret")
	}

	pool := newRelayPool(relayCfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := pool.Shutdown(ctx); err != nil {
			t.Logf("drain relay pool: %v", err)
		}
	})

	repo := repository.NewPostgresRepository(qaDB)
	return &server{
		repo:         repo,
		jwtSecret:    []byte("qa-jwt-secret-with-at-least-32-chars!!"),
		metaClient:   &http.Client{Transport: stub, Timeout: 10 * time.Second},
		metaAPIVer:   "v21.0",
		usage:        service.NewUsageService(repo),
		rateLimiter:  security.NewRateLimiter(maxMessagesPerMinute, 15*time.Minute),
		relay:        pool,
		rejectionLog: newLogSampler(defaultRejectionLogInterval),
	}
}

// qaMetaAppSecret devolve o segredo que o harness realmente injetou. Fixar
// "qa-app-secret" no teste dava vermelho falso em máquina/CI com META_APP_SECRET
// real exportado: o harness preserva o valor do ambiente e o handler devolveria
// 401 por MAC divergente.
func qaMetaAppSecret(t *testing.T) string {
	t.Helper()
	secret := strings.TrimSpace(os.Getenv("META_APP_SECRET"))
	if secret == "" {
		secret = "qa-app-secret"
	}
	return secret
}

// ----------------------------------------------------------------------------
// Fixtures
// ----------------------------------------------------------------------------

type qaSystem struct {
	model.System
	APIKey string
}

func createQASystem(t *testing.T, name, slug, webhookURL string) qaSystem {
	t.Helper()

	apiKey := "sk_live_qa_" + slug
	system := model.System{
		Name:       name,
		Slug:       slug,
		APIKeyHash: security.HashAPIKey(apiKey),
		WebhookURL: webhookURL,
	}
	if err := repository.NewPostgresRepository(qaDB).CreateSystem(context.Background(), &system); err != nil {
		t.Fatalf("create system %s: %v", slug, err)
	}
	return qaSystem{System: system, APIKey: apiKey}
}

func createQAConnection(
	t *testing.T,
	system qaSystem,
	tenantID, phoneNumberID, accessToken, webhookURL string,
) *model.WhatsAppConnection {
	t.Helper()

	conn := &model.WhatsAppConnection{
		SystemID:            system.ID,
		SistemaOrigem:       system.Slug,
		TenantID:            tenantID,
		WabaID:              "waba-" + tenantID,
		PhoneNumberID:       phoneNumberID,
		AccessToken:         accessToken,
		WebhookURL:          webhookURL,
		WebhookSecret:       "qa-webhook-secret-" + tenantID,
		WhatsAppPhoneNumber: "5511" + phoneNumberID,
		Status:              model.ConnectionStatusActive,
	}
	if err := repository.NewPostgresRepository(qaDB).CreateWhatsAppConnection(context.Background(), conn); err != nil {
		t.Fatalf("create connection %s/%s: %v", system.Slug, tenantID, err)
	}
	return conn
}

// ----------------------------------------------------------------------------
// Chamadas HTTP
// ----------------------------------------------------------------------------

func qaPostSendNotification(t *testing.T, srv *server, apiKey string, payload any) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal send-notification payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/send-notification", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set(apiKeyHeader, apiKey)
	}

	rec := httptest.NewRecorder()
	srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendNotification)).ServeHTTP(rec, req)
	return rec
}

func qaPostMetaWebhook(t *testing.T, srv *server, rawPayload string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(rawPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", security.SignMetaPayload(qaMetaAppSecret(t), []byte(rawPayload)))

	rec := httptest.NewRecorder()
	srv.handleWhatsAppWebhookEvent(rec, req)
	return rec
}

// ----------------------------------------------------------------------------
// Consultas de auditoria em message_logs
// ----------------------------------------------------------------------------

type qaMessageLogRow struct {
	ID              string
	SystemID        string
	ConnectionID    string
	SistemaOrigem   string
	TenantID        string
	MetaMessageID   string
	PhoneNumber     string
	TemplateName    string
	ReceivedContent string
	Direction       string
	Category        string
	Status          string
	FailureReason   string
	MetaCost        float64
	DeliveredAt     *time.Time
}

func qaListMessageLogs(t *testing.T) []qaMessageLogRow {
	t.Helper()

	rows, err := qaDB.Query(`
		SELECT id::text, COALESCE(system_id::text, ''), COALESCE(connection_id::text, ''), COALESCE(sistema_origem, ''),
		       COALESCE(external_client_id, ''), COALESCE(meta_message_id, ''), phone_number,
		       COALESCE(template_name, ''), COALESCE(received_content, ''), direction,
		       COALESCE(message_category, ''), status, COALESCE(failure_reason, ''),
		       meta_cost, delivered_at
		FROM message_logs
		ORDER BY created_at ASC, id ASC
	`)
	if err != nil {
		t.Fatalf("list message logs: %v", err)
	}
	defer rows.Close()

	logs := make([]qaMessageLogRow, 0)
	for rows.Next() {
		var row qaMessageLogRow
		if err := rows.Scan(
			&row.ID, &row.SystemID, &row.ConnectionID, &row.SistemaOrigem, &row.TenantID, &row.MetaMessageID,
			&row.PhoneNumber, &row.TemplateName, &row.ReceivedContent, &row.Direction,
			&row.Category, &row.Status, &row.FailureReason, &row.MetaCost, &row.DeliveredAt,
		); err != nil {
			t.Fatalf("scan message log: %v", err)
		}
		logs = append(logs, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate message logs: %v", err)
	}
	return logs
}

// qaInsertPendingInbound grava direto no banco uma linha inbound presa em
// pending com created_at arbitrário — simula o repasse que morreu no meio do voo
// (deploy/restart) e que o sweep precisa reconciliar.
func qaInsertPendingInbound(
	t *testing.T,
	systemID, connectionID, sistemaOrigem, externalClientID, metaMessageID string,
	phoneNumber, eventType, content string,
	createdAt time.Time,
) string {
	t.Helper()

	var connection any
	if connectionID != "" {
		connection = connectionID
	}
	var sistema any
	if sistemaOrigem != "" {
		sistema = sistemaOrigem
	}

	var id string
	err := qaDB.QueryRow(`
		INSERT INTO message_logs (
			system_id, connection_id, sistema_origem, external_client_id, meta_message_id,
			appointment_id, phone_number, template_name, received_content,
			direction, status, created_at
		)
		VALUES ($1, $2, $3, $4, $5, '-', $6, $7, $8, 'INBOUND', 'pending', $9)
		RETURNING id::text
	`, systemID, connection, sistema, externalClientID, metaMessageID,
		phoneNumber, eventType, content, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert pending inbound log: %v", err)
	}
	return id
}

func qaMessageLogStatus(t *testing.T, logID string) string {
	t.Helper()
	var status string
	if err := qaDB.QueryRow(`SELECT status FROM message_logs WHERE id = $1`, logID).Scan(&status); err != nil {
		t.Fatalf("read message log status: %v", err)
	}
	return status
}

func qaMessageLogFailureReason(t *testing.T, logID string) string {
	t.Helper()
	var reason sql.NullString
	if err := qaDB.QueryRow(`SELECT failure_reason FROM message_logs WHERE id = $1`, logID).Scan(&reason); err != nil {
		t.Fatalf("read failure_reason: %v", err)
	}
	if !reason.Valid {
		return ""
	}
	return reason.String
}

func qaMessageLogInboundPayload(t *testing.T, logID string) []byte {
	t.Helper()
	var raw []byte
	if err := qaDB.QueryRow(`SELECT inbound_payload FROM message_logs WHERE id = $1`, logID).Scan(&raw); err != nil {
		t.Fatalf("read inbound_payload: %v", err)
	}
	return raw
}

func qaCountMessageLogs(t *testing.T) int {
	t.Helper()
	var count int
	if err := qaDB.QueryRow(`SELECT COUNT(*) FROM message_logs`).Scan(&count); err != nil {
		t.Fatalf("count message logs: %v", err)
	}
	return count
}

func qaConnectionStatus(t *testing.T, connectionID string) string {
	t.Helper()
	var status string
	err := qaDB.QueryRow(`SELECT status FROM whatsapp_connections WHERE id = $1`, connectionID).Scan(&status)
	if err != nil {
		t.Fatalf("read connection status: %v", err)
	}
	return status
}

// qaWaitFor espera por uma condição eventual (processamento assíncrono do webhook).
func qaWaitFor(t *testing.T, timeout time.Duration, describe string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout after %s waiting for: %s", timeout, describe)
}

func qaDecodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		return strings.TrimSpace(rec.Body.String())
	}
	return payload.Error
}
