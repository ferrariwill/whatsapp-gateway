package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiKeyHeader = "X-API-Key"

// Client é o SDK HTTP para consumir o WhatsApp Gateway multi-tenant.
type Client struct {
	BaseURL      string
	APIKey       string
	TemplateName string
	HTTPClient   *http.Client
}

// NewClient cria um cliente pronto para disparar mensagens via Gateway.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type sendTemplateRequest struct {
	ExternalClientID string   `json:"external_client_id"`
	PhoneNumber      string   `json:"phone_number"`
	AppointmentID    string   `json:"appointment_id"`
	TemplateName     string   `json:"template_name"`
	Variables        []string `json:"variables"`
}

type sendTemplateResponse struct {
	MessageLogID string `json:"message_log_id"`
	Status       string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// SendAppointmentConfirmation envia um template de confirmação de agendamento
// para o endpoint POST /v1/messages/send-template do Gateway.
func (c *Client) SendAppointmentConfirmation(
	ctx context.Context,
	externalClientID string,
	idAgendamento string,
	telefoneCliente string,
	variaveisTemplate []string,
) error {
	if c == nil {
		return fmt.Errorf("whatsapp client is nil")
	}
	if c.BaseURL == "" {
		return fmt.Errorf("base URL is required")
	}
	if c.APIKey == "" {
		return fmt.Errorf("api key is required")
	}
	if c.TemplateName == "" {
		return fmt.Errorf("template name is required")
	}

	externalClientID = strings.TrimSpace(externalClientID)
	idAgendamento = strings.TrimSpace(idAgendamento)
	telefoneCliente = strings.TrimSpace(telefoneCliente)
	if externalClientID == "" || idAgendamento == "" || telefoneCliente == "" {
		return fmt.Errorf("external client id, appointment id and phone number are required")
	}

	payload := sendTemplateRequest{
		ExternalClientID: externalClientID,
		PhoneNumber:      telefoneCliente,
		AppointmentID:    idAgendamento,
		TemplateName:     c.TemplateName,
		Variables:        variaveisTemplate,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := c.BaseURL + "/v1/messages/send-template"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, c.APIKey)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return parseGatewayError(resp.StatusCode, respBody)
	}

	var success sendTemplateResponse
	if err := json.Unmarshal(respBody, &success); err != nil {
		return fmt.Errorf("decode success response: %w", err)
	}

	if success.Status == "failed" {
		return fmt.Errorf("gateway returned failed status for message log %s", success.MessageLogID)
	}

	return nil
}

func parseGatewayError(statusCode int, body []byte) error {
	var gatewayErr errorResponse
	if err := json.Unmarshal(body, &gatewayErr); err == nil && gatewayErr.Error != "" {
		return fmt.Errorf("gateway error (status %d): %s", statusCode, gatewayErr.Error)
	}

	return fmt.Errorf("gateway error (status %d): %s", statusCode, strings.TrimSpace(string(body)))
}
