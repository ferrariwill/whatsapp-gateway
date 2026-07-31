package main

import (
	"fmt"

	"html/template"

	"net/http"

	"os"

	"path/filepath"

	"strconv"

	"time"

	"github.com/whatsappgetway/gateway/internal/model"

	"github.com/whatsappgetway/gateway/internal/repository"

	"github.com/whatsappgetway/gateway/internal/security"

	"github.com/whatsappgetway/gateway/internal/service"
)

type pageData struct {
	Title string

	Error string
}

type loginPageData struct {
	pageData
}

type dashboardPageData struct {
	pageData

	Systems       []systemRowData
	SystemOptions []systemOptionData

	HasSystems bool

	Connections []connectionRowData

	Channels []channelRowData

	AuditLogs []auditLogRowData

	UsageMonth int

	UsageYear int
}

type systemRowData struct {
	ID         string
	Name       string
	Slug       string
	WebhookURL string
	APIKeyHint string
	CreatedAt  string
}

type systemOptionData struct {
	ID   string
	Name string
	Slug string
}

type connectionRowData struct {
	ConnectionID        string
	SystemID            string
	SystemName          string
	SystemSlug          string
	SistemaOrigem       string
	TenantID            string
	WabaID              string
	PhoneNumberID       string
	AccessTokenHint     string
	WebhookURL          string
	WhatsAppPhoneNumber string
	Status              string
	IsSuspendedSpam     bool
	CreatedAt           string
}

type channelRowData struct {
	SystemID string

	SystemName string

	ChannelID string

	ChannelLabel string

	ExternalClientID string

	WhatsAppPhoneNumber string

	PhoneNumberID string

	Status string

	IsSuspendedSpam bool

	CreatedAt string
}

type auditLogRowData struct {
	CreatedAt        string
	SystemName       string
	ExternalClientID string
	Direction        string
	IsInbound        bool
	PhoneNumber      string
	TemplateLabel    string
	Content          string
	SpamAlert        bool
	Status           string
	FailureReason    string
	IsRejected       bool
}

type externalClientUsageView struct {
	SystemName         string
	ExternalClientID   string
	DisplayLabel       string
	TotalMessagesSent  int64
	TotalDelivered     int64
	TotalFailed        int64
	EstimatedMetaCost  string
	WithinMonthlyLimit bool
	MonthlyLimit       int64
	BarWidthPercent    int
}

type usageVolumeView struct {
	Month        int
	Year         int
	MonthlyLimit int64
	Clients      []externalClientUsageView
	HasData      bool
	TotalSent    int64
}

func loadTemplates() *template.Template {

	dir := resolveTemplatesDir()

	tpl := template.New("")

	tpl = template.Must(tpl.ParseGlob(filepath.Join(dir, "*.html")))

	tpl = template.Must(tpl.ParseGlob(filepath.Join(dir, "partials", "*.html")))

	return tpl

}

func resolveTemplatesDir() string {

	if dir := os.Getenv("TEMPLATES_DIR"); dir != "" {

		return dir

	}

	candidates := []string{

		filepath.Join("..", "..", "frontend", "templates"),

		filepath.Join("..", "frontend", "templates"),

		filepath.Join("frontend", "templates"),

		"templates",
	}

	for _, candidate := range candidates {

		if info, err := os.Stat(candidate); err == nil && info.IsDir() {

			return candidate

		}

	}

	return filepath.Join("..", "..", "frontend", "templates")

}

func buildDashboardData(
	systems []model.System,
	connections []model.WhatsAppConnection,
	channels []model.ClientChannelWithSystem,
	logs []repository.MessageLogWithSystem,
	spamKeys map[string]struct{},
) dashboardPageData {

	now := time.Now()

	systemRows := make([]systemRowData, 0, len(systems))
	systemOptions := make([]systemOptionData, 0, len(systems))
	systemNames := make(map[string]string, len(systems))
	for _, sys := range systems {
		systemRows = append(systemRows, newSystemRow(sys))
		systemOptions = append(systemOptions, systemOptionData{
			ID:   sys.ID,
			Name: sys.Name,
			Slug: sys.Slug,
		})
		systemNames[sys.ID] = sys.Name
	}

	connectionRows := make([]connectionRowData, 0, len(connections))
	for _, conn := range connections {
		systemName := systemNames[conn.SystemID]
		if systemName == "" {
			systemName = conn.SistemaOrigem
		}
		connectionRows = append(connectionRows, newConnectionRow(conn, systemName))
	}

	channelRows := make([]channelRowData, 0, len(channels))

	for _, ch := range channels {

		channelRows = append(channelRows, channelRowData{

			SystemID: ch.SystemID,

			SystemName: ch.SystemName,

			ChannelID: ch.ID,

			ChannelLabel: ch.SalonName,

			ExternalClientID: ch.ExternalClientID,

			WhatsAppPhoneNumber: ch.WhatsAppPhoneNumber,

			PhoneNumberID: ch.PhoneNumberID,

			Status: ch.Status,

			IsSuspendedSpam: ch.Status == string(security.ClientChannelStatusSuspendedSpam),

			CreatedAt: formatDateTime(ch.CreatedAt),
		})

	}

	auditRows := make([]auditLogRowData, 0, len(logs))
	for _, entry := range logs {
		key := clientAuditKey(entry.SystemID, entry.ExternalClientID)
		_, spamAlert := spamKeys[key]
		auditRows = append(auditRows, auditLogRowData{
			CreatedAt:        formatDateTime(entry.CreatedAt),
			SystemName:       entry.SystemName,
			ExternalClientID: entry.ExternalClientID,
			Direction:        string(entry.Direction),
			IsInbound:        entry.Direction == model.MessageDirectionInbound,
			PhoneNumber:      entry.PhoneNumber,
			TemplateLabel:    auditTemplateLabel(entry.MessageLog),
			Content:          auditDisplayContent(entry.MessageLog),
			SpamAlert:        spamAlert,
			Status:           string(entry.Status),
			FailureReason:    entry.FailureReason,
			IsRejected:       entry.Status == model.MessageStatusRejected,
		})
	}

	return dashboardPageData{
		pageData:      pageData{Title: "Dashboard — Volume de Disparos por Aplicação e Clientes"},
		Systems:       systemRows,
		SystemOptions: systemOptions,
		HasSystems:    len(systemRows) > 0,
		Connections:   connectionRows,
		Channels:      channelRows,
		AuditLogs:     auditRows,
		UsageMonth:    int(now.Month()),
		UsageYear:     now.Year(),
	}
}

func newSystemRow(system model.System) systemRowData {
	hint := system.APIKeyHash
	if len(hint) > 8 {
		hint = "…" + hint[len(hint)-8:]
	}
	return systemRowData{
		ID:         system.ID,
		Name:       system.Name,
		Slug:       system.Slug,
		WebhookURL: system.WebhookURL,
		APIKeyHint: hint,
		CreatedAt:  formatDateTime(system.CreatedAt),
	}
}

func systemAPIKeyRevealHTML(apiKey string) string {
	return fmt.Sprintf(
		`<div id="system-api-key-reveal" hx-swap-oob="innerHTML" class="mb-4 rounded-xl border border-amber-500/40 bg-amber-500/10 p-4">`+
			`<p class="text-sm font-semibold text-amber-200">Chave de API gerada — copie agora, ela não será exibida novamente:</p>`+
			`<div class="mt-2 flex flex-wrap items-center gap-2">`+
			`<code class="min-w-0 flex-1 rounded-lg bg-slate-950 px-3 py-2 font-mono text-xs text-emerald-300 break-all">%s</code>`+
			`<button type="button" data-copy="%s" onclick="copyToClipboard(this)" class="inline-flex min-h-[44px] min-w-[44px] items-center justify-center rounded-lg bg-emerald-500 px-4 py-2 text-sm font-semibold text-slate-950 hover:bg-emerald-400">Copiar</button>`+
			`</div></div>`,
		template.HTMLEscapeString(apiKey),
		template.HTMLEscapeString(apiKey),
	)
}

func systemSelectOptionOOB(systemID, name, slug string) string {
	escapedName := template.HTMLEscapeString(name)
	escapedSlug := template.HTMLEscapeString(slug)
	escapedID := template.HTMLEscapeString(systemID)
	label := fmt.Sprintf("%s (%s)", escapedName, escapedSlug)
	option := fmt.Sprintf(`<option value="%s">%s</option>`, escapedID, label)
	return fmt.Sprintf(
		`<template hx-swap-oob="beforeend:#add-channel-system-select">%s</template>`+
			`<template hx-swap-oob="beforeend:#edit-channel-system-select">%s</template>`+
			`<template hx-swap-oob="beforeend:#add-connection-system-select">%s</template>`+
			`<template hx-swap-oob="beforeend:#edit-connection-system-select">%s</template>`+
			`<template hx-swap-oob="outerHTML:#add-channel-btn"><button type="button" onclick="openModal('add-channel-modal')" class="inline-flex items-center justify-center rounded-xl bg-emerald-500 px-4 py-2.5 text-sm font-semibold text-slate-950 transition hover:bg-emerald-400">+ Vincular canal</button></template>`,
		option, option, option, option,
	)
}

func respondFormFeedback(w http.ResponseWriter, r *http.Request, targetID, message string) {
	if isHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("HX-Retarget", "#"+targetID)
		w.Header().Set("HX-Reswap", "innerHTML")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(channelModalErrorHTML(message)))
		return
	}
	writeHTML(w, http.StatusBadRequest, fmt.Sprintf(`<p class="text-red-300">%s</p>`, template.HTMLEscapeString(message)))
}

func respondSystemFormError(w http.ResponseWriter, r *http.Request, status int, message string) {
	respondFormFeedback(w, r, "system-form-feedback", message)
	_ = status
}

func newConnectionRow(conn model.WhatsAppConnection, systemName string) connectionRowData {
	status := conn.Status
	if status == "" {
		status = model.ConnectionStatusActive
	}
	tokenHint := conn.AccessToken
	if len(tokenHint) > 8 {
		tokenHint = "…" + tokenHint[len(tokenHint)-8:]
	}
	if systemName == "" {
		systemName = conn.SistemaOrigem
	}
	return connectionRowData{
		ConnectionID:        conn.ID,
		SystemID:            conn.SystemID,
		SystemName:          systemName,
		SystemSlug:          conn.SistemaOrigem,
		SistemaOrigem:       conn.SistemaOrigem,
		TenantID:            conn.TenantID,
		WabaID:              conn.WabaID,
		PhoneNumberID:       conn.PhoneNumberID,
		AccessTokenHint:     tokenHint,
		WebhookURL:          conn.WebhookURL,
		WhatsAppPhoneNumber: conn.WhatsAppPhoneNumber,
		Status:              status,
		IsSuspendedSpam:     status == model.ConnectionStatusSuspendedSpam,
		CreatedAt:           formatDateTime(conn.CreatedAt),
	}
}

func newChannelRow(systemID, systemName, channelID, channelLabel, externalClientID, whatsappPhoneNumber, phoneNumberID, status string, createdAt time.Time) channelRowData {
	if status == "" {
		status = string(security.ClientChannelStatusActive)
	}

	return channelRowData{

		SystemID: systemID,

		SystemName: systemName,

		ChannelID: channelID,

		ChannelLabel: channelLabel,

		ExternalClientID: externalClientID,

		WhatsAppPhoneNumber: whatsappPhoneNumber,

		PhoneNumberID: phoneNumberID,

		Status: status,

		IsSuspendedSpam: status == string(security.ClientChannelStatusSuspendedSpam),

		CreatedAt: formatDateTime(createdAt),
	}

}

func formatDateTime(value time.Time) string {

	if value.IsZero() {

		return "—"

	}

	return value.Local().Format("02/01/2006 15:04")

}

func loginErrorMessage(code string) string {

	switch code {

	case "invalid":

		return "E-mail ou senha inválidos."

	case "internal":

		return "Erro interno. Tente novamente."

	default:

		return ""

	}

}

func isHTMX(r *http.Request) bool {

	return r.Header.Get("HX-Request") == "true"

}

func renderLoginPage(w http.ResponseWriter, tpl *template.Template, errorMsg string) {

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	_ = tpl.ExecuteTemplate(w, "login.html", loginPageData{

		pageData: pageData{

			Title: "Login — WhatsApp Gateway",

			Error: errorMsg,
		},
	})

}

func renderDashboardPage(w http.ResponseWriter, tpl *template.Template, data dashboardPageData) {

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	_ = tpl.ExecuteTemplate(w, "dashboard.html", data)

}

func channelFormErrorHTML(message string) string {

	return fmt.Sprintf(`<tr><td colspan="8" class="px-4 py-3 text-sm text-red-300">%s</td></tr>`, template.HTMLEscapeString(message))

}

func channelModalErrorHTML(message string) string {
	return fmt.Sprintf(
		`<div class="rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-300">%s</div>`,
		template.HTMLEscapeString(message),
	)
}

func respondChannelFormError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if isHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("HX-Retarget", "#channel-form-feedback")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(channelModalErrorHTML(message)))
		return
	}

	writeHTML(w, status, channelFormErrorHTML(message))
}

func buildUsageVolumeView(reports []service.ExternalClientUsageReport, month, year int, monthlyLimit int64) usageVolumeView {
	view := usageVolumeView{
		Month:        month,
		Year:         year,
		MonthlyLimit: monthlyLimit,
	}

	for _, report := range reports {
		view.TotalSent += report.TotalMessagesSent
		view.Clients = append(view.Clients, externalClientUsageView{
			SystemName:         report.SystemName,
			ExternalClientID:   report.ExternalClientID,
			DisplayLabel:       report.SystemName + " — Cliente #" + report.ExternalClientID,
			TotalMessagesSent:  report.TotalMessagesSent,
			TotalDelivered:     report.TotalDelivered,
			TotalFailed:        report.TotalFailed,
			EstimatedMetaCost:  formatMoney(report.EstimatedMetaCost),
			WithinMonthlyLimit: report.WithinMonthlyLimit,
			MonthlyLimit:       report.MonthlyLimit,
			BarWidthPercent:    report.BarWidthPercent,
		})
	}

	view.HasData = len(view.Clients) > 0
	return view
}

func renderUsageVolumePartial(w http.ResponseWriter, tpl *template.Template, data usageVolumeView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "usage_volume.html", data); err != nil {
		_, _ = w.Write([]byte(usageVolumeErrorHTML("Erro ao renderizar volume de disparos.")))
	}
}

func usageVolumeErrorHTML(message string) string {
	return fmt.Sprintf(
		`<div class="rounded-xl border border-red-500/30 bg-red-500/10 p-4" role="alert">`+
			`<p class="text-sm text-red-200">%s</p>`+
			`<button type="button"`+
			` class="mt-3 inline-flex min-h-[44px] items-center justify-center rounded-xl border border-slate-700 bg-slate-950 px-4 py-2 text-sm text-slate-200 hover:bg-slate-800"`+
			` hx-get="/admin/usage/volume"`+
			` hx-include="#usage-filter-form"`+
			` hx-target="#usage-volume-card"`+
			` hx-swap="innerHTML"`+
			` hx-indicator="#usage-volume-loading">Tentar novamente</button>`+
			`</div>`,
		template.HTMLEscapeString(message),
	)
}

func writeUsageVolumeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(usageVolumeErrorHTML(message)))
}

func writeDashboardLoadError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Erro — WhatsApp Gateway</title>
<script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-full bg-slate-950 text-slate-100 antialiased">
  <main class="mx-auto flex min-h-screen max-w-lg flex-col items-center justify-center px-4 text-center">
    <div class="w-full rounded-2xl border border-red-500/30 bg-red-500/10 p-6" role="alert">
      <h1 class="text-lg font-semibold text-red-100">Não foi possível carregar o painel</h1>
      <p class="mt-2 text-sm text-red-200/90">%s</p>
      <a href="/dashboard" class="mt-4 inline-flex min-h-[44px] items-center justify-center rounded-xl bg-emerald-500 px-4 py-2.5 text-sm font-semibold text-slate-950 hover:bg-emerald-400">Tentar novamente</a>
    </div>
  </main>
</body>
</html>`, template.HTMLEscapeString(message))
}

func formatMoney(value float64) string {

	return strconv.FormatFloat(value, 'f', 4, 64)

}
