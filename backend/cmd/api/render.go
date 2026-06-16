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

	Channels    []channelRowData

	AuditLogs   []auditLogRowData

	UsageMonth int

	UsageYear  int

}



type channelRowData struct {

	SystemName          string

	ChannelID           string

	ChannelLabel        string

	ExternalClientID    string

	WhatsAppPhoneNumber string

	PhoneNumberID       string

	Status              string

	IsSuspendedSpam     bool

	CreatedAt           string

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

		filepath.Join("..", "frontend", "templates"),

		"templates",

		filepath.Join("frontend", "templates"),

	}



	for _, candidate := range candidates {

		if info, err := os.Stat(candidate); err == nil && info.IsDir() {

			return candidate

		}

	}



	return filepath.Join("..", "frontend", "templates")

}



func buildDashboardData(
	channels []model.ClientChannelWithSystem,
	logs []repository.MessageLogWithSystem,
	spamKeys map[string]struct{},
) dashboardPageData {

	now := time.Now()

	channelRows := make([]channelRowData, 0, len(channels))

	for _, ch := range channels {

		channelRows = append(channelRows, channelRowData{

			SystemName:          ch.SystemName,

			ChannelID:           ch.ID,

			ChannelLabel:        ch.SalonName,

			ExternalClientID:    ch.ExternalClientID,

			WhatsAppPhoneNumber: ch.WhatsAppPhoneNumber,

			PhoneNumberID:       ch.PhoneNumberID,

			Status:              ch.Status,

			IsSuspendedSpam:     ch.Status == string(security.ClientChannelStatusSuspendedSpam),

			CreatedAt:           formatDateTime(ch.CreatedAt),

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
		})
	}

	return dashboardPageData{

		pageData: pageData{Title: "Dashboard — Volume de Disparos por Aplicação e Clientes"},

		Channels: channelRows,

		AuditLogs: auditRows,

		UsageMonth: int(now.Month()),

		UsageYear:  now.Year(),

	}

}



func newChannelRow(systemName, channelID, channelLabel, externalClientID, whatsappPhoneNumber, phoneNumberID, status string, createdAt time.Time) channelRowData {
	if status == "" {
		status = string(security.ClientChannelStatusActive)
	}

	return channelRowData{

		SystemName:          systemName,

		ChannelID:           channelID,

		ChannelLabel:        channelLabel,

		ExternalClientID:    externalClientID,

		WhatsAppPhoneNumber: whatsappPhoneNumber,

		PhoneNumberID:       phoneNumberID,

		Status:              status,

		IsSuspendedSpam:     status == string(security.ClientChannelStatusSuspendedSpam),

		CreatedAt:           formatDateTime(createdAt),

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
		_, _ = w.Write([]byte(`<p class="text-sm text-red-300">Erro ao renderizar volume de disparos.</p>`))
	}
}



func formatMoney(value float64) string {

	return strconv.FormatFloat(value, 'f', 4, 64)

}


