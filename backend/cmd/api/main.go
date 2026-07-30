package main

import (
	"context"

	"database/sql"

	"encoding/json"

	"errors"

	"fmt"

	"html/template"

	"log"

	"net/http"

	"os"

	"os/signal"

	"strings"

	"syscall"

	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/whatsappgetway/gateway/internal/envutil"

	"github.com/whatsappgetway/gateway/internal/model"

	"github.com/whatsappgetway/gateway/internal/provider"

	"github.com/whatsappgetway/gateway/internal/repository"

	"github.com/whatsappgetway/gateway/internal/security"

	"github.com/whatsappgetway/gateway/internal/service"
)

type contextKey int

const (
	systemContextKey contextKey = iota

	userContextKey
)

const apiKeyHeader = "X-API-Key"

// defaultShutdownTimeout é o prazo total para parar de aceitar requisições e
// drenar os repasses inbound em voo.
const defaultShutdownTimeout = 20 * time.Second

type server struct {
	repo *repository.PostgresRepository

	templates *template.Template

	jwtSecret []byte

	metaClient *http.Client

	metaAPIVer string

	usage *service.UsageService

	rateLimiter *security.RateLimiter

	// relay limita a concorrência do processamento inbound e permite drenar os
	// repasses em voo no shutdown. Nil volta ao comportamento de uma goroutine
	// por evento (usado em testes unitários que montam o server na mão).
	relay *relayPool

	// rejectionLog amostra as linhas de log dos caminhos de rejeição dos
	// webhooks públicos, que qualquer origem pode disparar em volume.
	rejectionLog *logSampler
}

// logRejectedWebhook registra uma rejeição de webhook com amostragem.
func (s *server) logRejectedWebhook(format string, args ...any) {

	if s.rejectionLog == nil {

		log.Printf(format, args...)

		return

	}

	s.rejectionLog.Printf(format, args...)

}

type sendTemplateRequest struct {
	ExternalClientID string `json:"external_client_id"`

	PhoneNumber string `json:"phone_number"`

	AppointmentID string `json:"appointment_id"`

	TemplateName string `json:"template_name"`

	Variables []string `json:"variables"`
}

type sendTemplateResponse struct {
	MessageLogID string `json:"message_log_id"`

	Status model.MessageStatus `json:"status"`
}

type createTemplateRequest struct {
	Name string `json:"name"`

	Category string `json:"category"`

	TextBody string `json:"text_body"`

	Buttons []string `json:"buttons"`

	SistemaOrigem string `json:"sistema_origem"`

	TenantID string `json:"tenant_id"`
}

type createTemplateResponse struct {
	TemplateID string `json:"template_id"`

	Status string `json:"status"`
}

type listTemplatesResponse struct {
	Templates []provider.MetaTemplateResponse `json:"templates"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {

	databaseURL, err := envutil.ResolveDatabaseURL()

	if err != nil {

		log.Fatal(err)

	}

	if err := runMigrations(databaseURL); err != nil {

		log.Fatalf("run migrations: %v", err)

	}

	jwtSecret, err := loadJWTSecret(os.Getenv("JWT_SECRET"))

	if err != nil {

		log.Fatalf("invalid JWT_SECRET: %v", err)

	}

	db, err := sql.Open("pgx", databaseURL)

	if err != nil {

		log.Fatalf("open database: %v", err)

	}

	defer db.Close()

	db.SetMaxOpenConns(25)

	db.SetMaxIdleConns(5)

	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	defer cancel()

	if err := db.PingContext(ctx); err != nil {

		log.Fatalf("ping database: %v", err)

	}

	repo := repository.NewPostgresRepository(db)

	srv := &server{

		repo: repo,

		templates: loadTemplates(),

		jwtSecret: jwtSecret,

		metaClient: &http.Client{

			Timeout: 30 * time.Second,
		},

		metaAPIVer: os.Getenv("META_API_VERSION"),

		usage: service.NewUsageService(repo),

		rateLimiter: security.NewRateLimiterFromEnv(),

		relay: newRelayPool(relayPoolConfigFromEnv()),

		rejectionLog: newLogSampler(envDuration("WEBHOOK_REJECTION_LOG_INTERVAL", defaultRejectionLogInterval)),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)

	mux.HandleFunc("GET /login", srv.handleLoginPage)

	mux.HandleFunc("POST /login", srv.handleLogin)

	mux.HandleFunc("POST /logout", srv.handleLogout)

	mux.Handle("GET /dashboard", srv.jwtMiddleware(http.HandlerFunc(srv.handleDashboard)))

	mux.Handle("POST /admin/systems", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminCreateSystem)))

	mux.Handle("POST /admin/systems/{id}", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminUpdateSystem)))

	mux.Handle("POST /admin/systems/{id}/delete", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminDeleteSystem)))

	mux.Handle("POST /admin/channels", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminCreateChannel)))

	mux.Handle("POST /admin/channels/{id}", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminUpdateChannel)))

	mux.Handle("POST /admin/channels/{id}/delete", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminDeleteChannel)))

	mux.Handle("POST /admin/clients/{id}/unblock", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminUnblockClient)))

	mux.Handle("POST /admin/connections", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminCreateConnection)))
	mux.Handle("POST /admin/connections/{id}", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminUpdateConnection)))
	mux.Handle("POST /admin/connections/{id}/delete", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminDeleteConnection)))
	mux.Handle("POST /admin/connections/{id}/unblock", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminUnblockConnection)))

	mux.Handle("GET /admin/usage/volume", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminUsageVolume)))
	mux.Handle("GET /admin/audit/logs", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminAuditLogs)))
	mux.Handle("GET /admin/metrics/relay", srv.jwtMiddleware(http.HandlerFunc(srv.handleAdminRelayMetrics)))

	mux.HandleFunc("GET /webhook/whatsapp", srv.handleWhatsAppWebhookVerify)
	mux.HandleFunc("POST /webhook/whatsapp", srv.handleWhatsAppWebhookEvent)

	mux.HandleFunc("GET /meta/embedded-signup/callback", srv.handleEmbeddedSignupCallback)

	mux.Handle("POST /v1/embedded-signup/state", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleMintEmbeddedSignupState)))

	mux.HandleFunc("GET /webhooks/meta/{phone_number_id}", srv.handleMetaWebhookVerify)
	mux.HandleFunc("POST /webhooks/meta/{phone_number_id}", srv.handleMetaWebhookEvent)

	mux.Handle("POST /send-notification", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendNotification)))

	mux.Handle("POST /v1/channels", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleCreateChannel)))

	mux.Handle("POST /v1/messages/send-template", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleSendTemplate)))

	mux.Handle("POST /v1/templates", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleCreateTemplate)))

	mux.Handle("GET /v1/templates", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleListTemplates)))

	mux.Handle("GET /v1/usage/report", srv.apiKeyMiddleware(http.HandlerFunc(srv.handleUsageReport)))

	addr := envOrDefault("PORT", "8080")

	if !strings.HasPrefix(addr, ":") {

		addr = ":" + addr

	}

	corsCfg := loadCORSConfig()

	httpServer := &http.Server{

		Addr: addr,

		Handler: CORSMiddleware(corsCfg, mux),
	}

	// As rotinas de manutenção rodam enquanto o processo vive: o sweep fecha as
	// linhas pending que o restart anterior deixou órfãs, e o GC impede que
	// oauth_state_nonces cresça sem limite (uma linha por state emitido).
	sweepCtx, stopSweeper := context.WithCancel(context.Background())

	sweeperDone := make(chan struct{})

	go func() {

		defer close(sweeperDone)

		srv.runPendingSweeper(sweepCtx, pendingSweepConfigFromEnv())

	}()

	nonceGCDone := make(chan struct{})

	go func() {

		defer close(nonceGCDone)

		srv.runOAuthNonceGC(sweepCtx, oauthNonceGCConfigFromEnv())

	}()

	signals := make(chan os.Signal, 1)

	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	serverErr := make(chan error, 1)

	go func() {

		log.Printf("WhatsApp Gateway listening on %s", addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {

			serverErr <- err

		}

	}()

	select {

	case err := <-serverErr:

		stopSweeper()

		log.Fatalf("server failed: %v", err)

	case sig := <-signals:

		log.Printf("received %s, draining", sig)

	}

	shutdownTimeout := envDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)

	defer cancelShutdown()

	// Ordem importa: parar de aceitar requisições, drenar os repasses inbound já
	// admitidos (senão ficariam pending sem ninguém avisando o SaaS) e só então
	// encerrar o sweeper.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {

		log.Printf("http shutdown: %v", err)

	}

	if err := srv.relay.Shutdown(shutdownCtx); err != nil {

		log.Printf("inbound relay shutdown: %v", err)

	}

	stopSweeper()

	<-sweeperDone

	<-nonceGCDone

	log.Printf("WhatsApp Gateway stopped")

}

func handleHealth(w http.ResponseWriter, _ *http.Request) {

	w.WriteHeader(http.StatusOK)

	_, _ = w.Write([]byte("ok"))

}

func (s *server) jwtMiddleware(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		token, err := security.ReadAuthCookie(r)

		if err != nil {

			if isHTMX(r) {

				w.Header().Set("HX-Redirect", "/login")

				w.WriteHeader(http.StatusUnauthorized)

				return

			}

			http.Redirect(w, r, "/login", http.StatusSeeOther)

			return

		}

		userID, err := security.ValidateToken(token, s.jwtSecret)

		if err != nil {

			if isHTMX(r) {

				w.Header().Set("HX-Redirect", "/login")

				w.WriteHeader(http.StatusUnauthorized)

				return

			}

			http.Redirect(w, r, "/login", http.StatusSeeOther)

			return

		}

		ctx := context.WithValue(r.Context(), userContextKey, userID)

		next.ServeHTTP(w, r.WithContext(ctx))

	})

}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {

	renderLoginPage(w, s.templates, loginErrorMessage(r.URL.Query().Get("error")))

}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {

	email, password, ok := parseLoginCredentials(r)

	if !ok {

		s.respondLoginFailure(w, r, "invalid")

		return

	}

	user, err := s.repo.FindUserByEmail(r.Context(), email)

	if errors.Is(err, repository.ErrUserNotFound) {

		s.respondLoginFailure(w, r, "invalid")

		return

	}

	if err != nil {

		log.Printf("lookup user by email: %v", err)

		s.respondLoginFailure(w, r, "internal")

		return

	}

	if err := security.VerifyPassword(password, user.PasswordHash); err != nil {

		s.respondLoginFailure(w, r, "invalid")

		return

	}

	token, err := security.GenerateToken(user.ID, s.jwtSecret)

	if err != nil {

		log.Printf("generate jwt for user %s: %v", user.ID, err)

		s.respondLoginFailure(w, r, "internal")

		return

	}

	security.SetAuthCookie(w, token)

	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/dashboard")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)

}

func (s *server) respondLoginFailure(w http.ResponseWriter, r *http.Request, code string) {

	if isHTMX(r) {

		renderLoginPage(w, s.templates, loginErrorMessage(code))

		return

	}

	http.Redirect(w, r, "/login?error="+code, http.StatusSeeOther)

}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {

	security.ClearAuthCookie(w)

	http.Redirect(w, r, "/login", http.StatusSeeOther)

}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {

	if _, ok := userIDFromContext(r.Context()); !ok {

		http.Redirect(w, r, "/login", http.StatusSeeOther)

		return

	}

	s.renderDashboard(w, r)

}

func (s *server) renderDashboard(w http.ResponseWriter, r *http.Request) {

	systems, err := s.repo.ListSystems(r.Context())
	if err != nil {
		log.Printf("list systems: %v", err)
		http.Error(w, "failed to load systems", http.StatusInternalServerError)
		return
	}

	channels, err := s.repo.ListClientChannels(r.Context())

	if err != nil {

		log.Printf("list client channels: %v", err)

		http.Error(w, "failed to load channels", http.StatusInternalServerError)

		return

	}

	connections, err := s.repo.ListWhatsAppConnections(r.Context())
	if err != nil {
		log.Printf("list whatsapp connections: %v", err)
		http.Error(w, "failed to load connections", http.StatusInternalServerError)
		return
	}

	logs, err := s.repo.ListRecentMessageLogs(r.Context(), 50)

	if err != nil {

		log.Printf("list message logs: %v", err)

		http.Error(w, "failed to load message logs", http.StatusInternalServerError)

		return

	}

	spamKeys, err := s.repo.FindHighVolumeClientKeys(r.Context(), 10, 50)
	if err != nil {
		log.Printf("find high volume clients: %v", err)
		spamKeys = map[string]struct{}{}
	}

	renderDashboardPage(w, s.templates, buildDashboardData(systems, connections, channels, logs, spamKeys))

}

func (s *server) handleAdminCreateChannel(w http.ResponseWriter, r *http.Request) {

	if !s.requireAdminUser(w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {

		respondChannelFormError(w, r, http.StatusBadRequest, "formulário inválido")

		return

	}

	systemID := strings.TrimSpace(r.FormValue("system_id"))

	salonName := strings.TrimSpace(r.FormValue("salon_name"))

	externalClientID := strings.TrimSpace(r.FormValue("external_client_id"))

	whatsappPhoneNumber := strings.TrimSpace(r.FormValue("whatsapp_phone_number"))

	phoneNumberID := strings.TrimSpace(r.FormValue("phone_number_id"))

	if systemID == "" || salonName == "" || externalClientID == "" || whatsappPhoneNumber == "" || phoneNumberID == "" {

		respondChannelFormError(w, r, http.StatusBadRequest, "preencha todos os campos obrigatórios")

		return

	}

	system, err := s.repo.FindSystemByID(r.Context(), systemID)

	if errors.Is(err, repository.ErrSystemNotFound) {

		respondChannelFormError(w, r, http.StatusBadRequest, "selecione uma aplicação válida")

		return

	}

	if err != nil {

		log.Printf("find system %s: %v", systemID, err)

		respondChannelFormError(w, r, http.StatusInternalServerError, "erro ao carregar aplicação")

		return

	}

	channel := &model.ClientChannel{

		SystemID: system.ID,

		SalonName: salonName,

		ExternalClientID: externalClientID,

		PhoneNumberID: phoneNumberID,

		WhatsAppPhoneNumber: whatsappPhoneNumber,
	}

	if err := s.repo.CreateClientChannel(r.Context(), channel); err != nil {

		log.Printf("create client channel: %v", err)

		respondChannelFormError(w, r, http.StatusInternalServerError, "erro ao salvar canal WhatsApp — verifique se o Phone Number ID já não está em uso")

		return

	}

	row := newChannelRow(system.ID, system.Name, channel.ID, channel.SalonName, channel.ExternalClientID, channel.WhatsAppPhoneNumber, channel.PhoneNumberID, channel.Status, channel.CreatedAt)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := s.templates.ExecuteTemplate(w, "channel_row.html", row); err != nil {

		log.Printf("render channel row: %v", err)

		writeHTML(w, http.StatusInternalServerError, channelFormErrorHTML("canal salvo, mas falha ao renderizar linha"))

	}

}

func writeHTML(w http.ResponseWriter, status int, html string) {

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	w.WriteHeader(status)

	_, _ = w.Write([]byte(html))

}

func parseLoginCredentials(r *http.Request) (email, password string, ok bool) {

	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {

		var payload struct {
			Email string `json:"email"`

			Password string `json:"password"`
		}

		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {

			return "", "", false

		}

		email = strings.TrimSpace(payload.Email)

		password = payload.Password

	} else {

		if err := r.ParseForm(); err != nil {

			return "", "", false

		}

		email = strings.TrimSpace(r.FormValue("email"))

		password = r.FormValue("password")

	}

	if email == "" || password == "" {

		return "", "", false

	}

	return email, password, true

}

func userIDFromContext(ctx context.Context) (string, bool) {

	userID, ok := ctx.Value(userContextKey).(string)

	return userID, ok && userID != ""

}

func (s *server) apiKeyMiddleware(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		apiKey := strings.TrimSpace(r.Header.Get(apiKeyHeader))

		if apiKey == "" {

			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing X-API-Key header"})

			return

		}

		hash := security.HashAPIKey(apiKey)

		system, err := s.repo.FindSystemByAPIKeyHash(r.Context(), hash)

		if errors.Is(err, repository.ErrSystemNotFound) {

			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid api key"})

			return

		}

		if err != nil {

			log.Printf("lookup system: %v", err)

			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})

			return

		}

		ctx := context.WithValue(r.Context(), systemContextKey, *system)

		next.ServeHTTP(w, r.WithContext(ctx))

	})

}

func (s *server) handleSendTemplate(w http.ResponseWriter, r *http.Request) {

	system, ok := systemFromContext(r.Context())

	if !ok {

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})

		return

	}

	var req sendTemplateRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})

		return

	}

	req.ExternalClientID = strings.TrimSpace(req.ExternalClientID)

	req.PhoneNumber = strings.TrimSpace(req.PhoneNumber)

	req.AppointmentID = strings.TrimSpace(req.AppointmentID)

	req.TemplateName = strings.TrimSpace(req.TemplateName)

	if req.ExternalClientID == "" || req.PhoneNumber == "" || req.AppointmentID == "" || req.TemplateName == "" {

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "external_client_id, phone_number, appointment_id and template_name are required"})

		return

	}

	channel, err := s.repo.FindClientChannelBySystemAndExternalClientID(r.Context(), system.ID, req.ExternalClientID)

	if errors.Is(err, repository.ErrClientChannelNotFound) {

		writeJSON(w, http.StatusNotFound, errorResponse{Error: "whatsapp channel not found for external_client_id"})

		return

	}

	if err != nil {

		log.Printf("lookup channel for system %s client %s: %v", system.ID, req.ExternalClientID, err)

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})

		return

	}

	conn, err := s.resolveConnectionForChannel(r.Context(), system, channel, req.ExternalClientID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: "whatsapp connection not configured — cadastre em whatsapp_connections ou use POST /send-notification",
		})
		return
	}
	if err != nil {
		log.Printf("resolve connection for system %s client %s: %v", system.ID, req.ExternalClientID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if !s.enforceConnectionSpamProtection(w, r, conn, outboundAttemptAudit{
		AppointmentID: req.AppointmentID,
		PhoneNumber:   req.PhoneNumber,
		TemplateName:  req.TemplateName,
		Variables:     req.Variables,
	}) {
		return
	}

	withinLimit, err := s.usage.CheckMonthlyLimit(r.Context(), system.ID, req.ExternalClientID)
	if err != nil {
		log.Printf("check monthly limit for system %s client %s: %v", system.ID, req.ExternalClientID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to verify monthly limit"})
		return
	}
	if !withinLimit {
		log.Printf("monthly limit exceeded for system %s client %s", system.ID, req.ExternalClientID)
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: fmt.Sprintf("monthly message limit of %d exceeded for this external client", s.usage.MonthlyLimit()),
		})
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)

	metaMessageID, sendErr := metaProvider.SendAppointmentTemplate(
		r.Context(),
		conn.AccessToken,
		conn.PhoneNumberID,
		req.PhoneNumber,
		req.TemplateName,
		req.Variables,
	)

	status := model.MessageStatusSent

	if sendErr != nil {

		status = model.MessageStatusFailed

		log.Printf("send template for system %s client %s appointment %s: %v", system.ID, req.ExternalClientID, req.AppointmentID, sendErr)

	}

	messageLog := &model.MessageLog{

		SystemID: system.ID,

		ConnectionID: conn.ID,

		SistemaOrigem: conn.SistemaOrigem,

		ExternalClientID: req.ExternalClientID,

		MetaMessageID: metaMessageID,

		AppointmentID: req.AppointmentID,

		PhoneNumber: req.PhoneNumber,

		TemplateName: req.TemplateName,

		SentContent: formatOutboundSentContent(req.TemplateName, req.Variables),

		Direction: model.MessageDirectionOutbound,

		MessageCategory: model.MessageCategoryUtility,

		Status: status,
	}

	if err := s.repo.CreateMessageLog(r.Context(), messageLog); err != nil {

		log.Printf("persist message log for system %s: %v", system.ID, err)

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to persist message log"})

		return

	}

	if sendErr != nil {

		writeJSON(w, http.StatusBadGateway, errorResponse{Error: sendErr.Error()})

		return

	}

	writeJSON(w, http.StatusOK, sendTemplateResponse{

		MessageLogID: messageLog.ID,

		Status: messageLog.Status,
	})

}

func (s *server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {

	system, ok := systemFromContext(r.Context())

	if !ok {

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})

		return

	}

	var req createTemplateRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})

		return

	}

	req.Name = strings.TrimSpace(req.Name)

	req.Category = strings.TrimSpace(req.Category)

	req.TextBody = strings.TrimSpace(req.TextBody)

	req.SistemaOrigem = strings.TrimSpace(strings.ToLower(req.SistemaOrigem))

	req.TenantID = strings.TrimSpace(req.TenantID)

	if req.Name == "" || req.TextBody == "" {

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "name and text_body are required"})

		return

	}

	if req.TenantID == "" {

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "tenant_id is required"})

		return

	}

	if req.SistemaOrigem != "" && req.SistemaOrigem != system.Slug {

		writeJSON(w, http.StatusForbidden, errorResponse{Error: "sistema_origem does not match authenticated system"})

		return

	}

	conn, err := s.repo.FindConnectionBySystemAndTenant(r.Context(), system.ID, req.TenantID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "whatsapp connection not found"})
		return
	}
	if err != nil {
		log.Printf("lookup connection %s/%s: %v", system.Slug, req.TenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)

	templateID, err := metaProvider.CreateTemplate(
		r.Context(),
		conn.AccessToken,
		conn.WabaID,
		req.Name,
		req.Category,
		req.TextBody,
		req.Buttons,
	)

	if err != nil {

		log.Printf("create template for system %s: %v", system.ID, err)

		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})

		return

	}

	writeJSON(w, http.StatusCreated, createTemplateResponse{

		TemplateID: templateID,

		Status: "PENDING",
	})

}

func (s *server) handleListTemplates(w http.ResponseWriter, r *http.Request) {

	system, ok := systemFromContext(r.Context())

	if !ok {

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "system not found in context"})

		return

	}

	_ = system

	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))

	if tenantID == "" {

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "tenant_id query param is required"})

		return

	}

	conn, err := s.repo.FindConnectionBySystemAndTenant(r.Context(), system.ID, tenantID)
	if errors.Is(err, repository.ErrConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "whatsapp connection not found"})
		return
	}
	if err != nil {
		log.Printf("lookup connection %s/%s: %v", system.Slug, tenantID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	metaProvider := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)

	templates, err := metaProvider.GetTemplatesStatus(r.Context(), conn.AccessToken, conn.WabaID)

	if err != nil {

		log.Printf("list templates for %s/%s: %v", system.Slug, tenantID, err)

		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})

		return

	}

	if templates == nil {

		templates = []provider.MetaTemplateResponse{}

	}

	writeJSON(w, http.StatusOK, listTemplatesResponse{Templates: templates})

}

func systemFromContext(ctx context.Context) (model.System, bool) {

	system, ok := ctx.Value(systemContextKey).(model.System)

	return system, ok

}

func writeJSON(w http.ResponseWriter, status int, payload any) {

	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(payload)

}

func envOrDefault(key, fallback string) string {

	if value := strings.TrimSpace(os.Getenv(key)); value != "" {

		return value

	}

	return fallback

}

func loadJWTSecret(raw string) ([]byte, error) {

	if strings.TrimSpace(raw) == "" {

		return nil, fmt.Errorf("JWT_SECRET is required")

	}

	if len(raw) < 32 {

		return nil, fmt.Errorf("JWT_SECRET must be at least 32 characters")

	}

	return []byte(raw), nil

}
