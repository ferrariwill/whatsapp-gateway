package model

import "time"

/*
Segurança de dados sensíveis (pacote internal/security):

  - Hash (SHA-256 via security.HashAPIKey):
      APIKeyHash → digest da chave de API do hub (sk_live_...)

  - AccessToken Meta (WhatsAppConnection.AccessToken):
      Armazenado por conexão/tenant no banco — nunca em variáveis globais do servidor.
*/

// WhatsAppConnection representa o chip WhatsApp de um tenant SaaS (multi-tenant).
type WhatsAppConnection struct {
	ID                  string    `json:"id" db:"id"`
	SystemID            string    `json:"system_id" db:"system_id"`
	SistemaOrigem       string    `json:"sistema_origem" db:"sistema_origem"`
	TenantID            string    `json:"tenant_id" db:"tenant_id"`
	WabaID              string    `json:"waba_id" db:"waba_id"`
	PhoneNumberID       string    `json:"phone_number_id" db:"phone_number_id"`
	AccessToken         string    `json:"-" db:"access_token"`
	WebhookURL          string    `json:"webhook_url,omitempty" db:"webhook_url"`
	WhatsAppPhoneNumber string    `json:"whatsapp_phone_number,omitempty" db:"whatsapp_phone_number"`
	Status              string    `json:"status" db:"status"`
	CreatedAt           time.Time `json:"created_at" db:"created_at"`
}

const (
	ConnectionStatusActive        = "ACTIVE"
	ConnectionStatusSuspendedSpam = "SUSPENDED_SPAM"
)

// ClientChannel representa o chip WhatsApp de um salão/clínica (profissional).
type ClientChannel struct {
	ID                  string    `json:"id" db:"id"`
	SystemID            string    `json:"system_id" db:"system_id"`
	SalonName           string    `json:"salon_name" db:"salon_name"`
	ExternalClientID    string    `json:"external_client_id" db:"external_client_id"`
	PhoneNumberID       string    `json:"phone_number_id" db:"phone_number_id"`
	WhatsAppPhoneNumber string    `json:"whatsapp_phone_number" db:"whatsapp_phone_number"`
	Status              string    `json:"status" db:"status"`
	CreatedAt           time.Time `json:"created_at" db:"created_at"`
}

// ClientChannelWithSystem inclui dados da aplicação mãe para roteamento de webhook.
type ClientChannelWithSystem struct {
	ClientChannel
	SystemName string
	WebhookURL string
}

// MessageStatus representa o ciclo de vida de uma mensagem no gateway.
type MessageStatus string

const (
	MessageStatusPending   MessageStatus = "pending"
	MessageStatusRelaying  MessageStatus = "relaying" // claim do sweep, antes do POST ao SaaS
	MessageStatusSent      MessageStatus = "sent"
	MessageStatusDelivered MessageStatus = "delivered"
	MessageStatusFailed    MessageStatus = "failed"
)

// System representa a aplicação mãe de agendamento (tenant raiz do gateway).
type System struct {
	ID         string    `json:"id" db:"id"`
	Name       string    `json:"name" db:"name"`
	Slug       string    `json:"slug" db:"slug"`
	APIKeyHash string    `json:"-" db:"api_key_hash"`
	WebhookURL string    `json:"webhook_url,omitempty" db:"webhook_url"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
}

// User representa um operador do painel web do gateway.
type User struct {
	ID           string    `json:"id" db:"id"`
	Email        string    `json:"email" db:"email"`
	PasswordHash string    `json:"-" db:"password_hash"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
}

// MessageCategory classifica a mensagem para fins de faturamento Meta.
type MessageCategory string

const (
	MessageCategoryUtility        MessageCategory = "UTILITY"
	MessageCategoryMarketing      MessageCategory = "MARKETING"
	MessageCategoryAuthentication MessageCategory = "AUTHENTICATION"
	MessageCategoryService        MessageCategory = "SERVICE"
)

// MessageDirection indica se o log é de envio ou recebimento.
type MessageDirection string

const (
	MessageDirectionOutbound MessageDirection = "OUTBOUND"
	MessageDirectionInbound  MessageDirection = "INBOUND"
)

// MessageLog registra tentativas de envio e respostas recebidas por system/cliente externo.
type MessageLog struct {
	ID               string           `json:"id" db:"id"`
	SystemID         string           `json:"system_id" db:"system_id"`
	ExternalClientID string           `json:"external_client_id,omitempty" db:"external_client_id"`
	MetaMessageID    string           `json:"meta_message_id,omitempty" db:"meta_message_id"`
	AppointmentID    string           `json:"appointment_id" db:"appointment_id"`
	ConnectionID     string           `json:"connection_id,omitempty" db:"connection_id"`
	SistemaOrigem    string           `json:"sistema_origem,omitempty" db:"sistema_origem"`
	PhoneNumber      string           `json:"phone_number" db:"phone_number"`
	TemplateName     string           `json:"template_name,omitempty" db:"template_name"`
	SentContent      string           `json:"sent_content,omitempty" db:"sent_content"`
	ReceivedContent  string           `json:"received_content,omitempty" db:"received_content"`
	Direction        MessageDirection `json:"direction" db:"direction"`
	MessageCategory  MessageCategory  `json:"message_category,omitempty" db:"message_category"`
	Status           MessageStatus    `json:"status" db:"status"`
	MetaCost         float64          `json:"meta_cost" db:"meta_cost"`
	DeliveredAt      *time.Time       `json:"delivered_at,omitempty" db:"delivered_at"`
	CreatedAt        time.Time        `json:"created_at" db:"created_at"`
}
