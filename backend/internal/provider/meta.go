package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	defaultAPIVersion   = "v21.0"
	defaultLanguageCode = "pt_BR"
	graphAPIBaseURL     = "https://graph.facebook.com"

	ButtonPayloadConfirm    = "APPT_CONFIRM"
	ButtonPayloadReschedule = "APPT_RESCHEDULE"
)

// MetaProvider comunica com a WhatsApp Cloud API usando credenciais por conexão/tenant.
type MetaProvider struct {
	client       *http.Client
	apiVersion   string
	languageCode string
}

// NewMetaProvider cria um provider stateless; o access token é passado em cada chamada.
func NewMetaProvider(client *http.Client, apiVersion string) *MetaProvider {
	if client == nil {
		client = http.DefaultClient
	}
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	return &MetaProvider{
		client:       client,
		apiVersion:   apiVersion,
		languageCode: defaultLanguageCode,
	}
}

// SendMessageRequest representa o corpo do POST /{phone-number-id}/messages.
type SendMessageRequest struct {
	MessagingProduct string   `json:"messaging_product"`
	To               string   `json:"to"`
	Type             string   `json:"type"`
	Template         Template `json:"template"`
}

// Template descreve um modelo aprovado na Meta Business Manager para envio.
type Template struct {
	Name       string              `json:"name"`
	Language   TemplateLanguage    `json:"language"`
	Components []TemplateComponent `json:"components,omitempty"`
}

// TemplateLanguage define o locale do template (ex.: pt_BR).
type TemplateLanguage struct {
	Code string `json:"code"`
}

// TemplateComponent agrupa parâmetros dinâmicos de body ou botões interativos no envio.
type TemplateComponent struct {
	Type       string              `json:"type"`
	SubType    string              `json:"sub_type,omitempty"`
	Index      string              `json:"index,omitempty"`
	Parameters []TemplateParameter `json:"parameters,omitempty"`
}

// TemplateParameter é um placeholder de body (text) ou botão (payload/text) no envio.
type TemplateParameter struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Payload string `json:"payload,omitempty"`
}

// MessageTemplateCreateRequest representa o payload de criação na Graph API.
type MessageTemplateCreateRequest struct {
	Name                string                           `json:"name"`
	Category            string                           `json:"category"`
	AllowCategoryChange bool                             `json:"allow_category_change"`
	Language            string                           `json:"language"`
	Components          []MessageTemplateCreateComponent `json:"components"`
}

// MessageTemplateCreateComponent descreve BODY ou BUTTONS na criação de templates.
type MessageTemplateCreateComponent struct {
	Type    string                  `json:"type"`
	Text    string                  `json:"text,omitempty"`
	Buttons []MessageTemplateButton `json:"buttons,omitempty"`
}

// MessageTemplateButton representa um botão QUICK_REPLY na criação de templates.
type MessageTemplateButton struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MetaTemplateResponse expõe metadados + components de um template da Meta.
type MetaTemplateResponse struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Status       string          `json:"status"`
	Category     string          `json:"category"`
	Language     string          `json:"language"`
	Components   json.RawMessage `json:"components,omitempty"`
	QualityScore string          `json:"quality_score,omitempty"`
}

// MetaAPIError representa falha tipada da Graph API (sync grava sem apagar catálogo).
type MetaAPIError struct {
	Status  int
	Code    int
	Message string
}

func (e *MetaAPIError) Error() string {
	if e == nil {
		return "meta api error"
	}
	if e.Code != 0 {
		return fmt.Sprintf("meta api error (status %d, code %d): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("meta api error (status %d): %s", e.Status, e.Message)
}

func AsMetaAPIError(err error) (*MetaAPIError, bool) {
	if err == nil {
		return nil, false
	}
	if me, ok := err.(*MetaAPIError); ok {
		return me, true
	}
	return nil, false
}

type createMessageTemplateResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type sendMessageResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

type listMessageTemplatesResponse struct {
	Data []struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Status       string          `json:"status"`
		Category     string          `json:"category"`
		Language     string          `json:"language"`
		Components   json.RawMessage `json:"components"`
		QualityScore json.RawMessage `json:"quality_score"`
	} `json:"data"`
	Paging struct {
		Next string `json:"next"`
	} `json:"paging"`
}

type metaErrorResponse struct {
	Error struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      int    `json:"code"`
		FBTraceID string `json:"fbtrace_id"`
	} `json:"error"`
}

func BodyComponent(vars []string) TemplateComponent {
	params := make([]TemplateParameter, len(vars))
	for i, v := range vars {
		params[i] = TemplateParameter{Type: "text", Text: v}
	}
	return TemplateComponent{Type: "body", Parameters: params}
}

func QuickReplyButton(index int, payload string) TemplateComponent {
	return TemplateComponent{
		Type:    "button",
		SubType: "quick_reply",
		Index:   fmt.Sprintf("%d", index),
		Parameters: []TemplateParameter{
			{Type: "payload", Payload: payload},
		},
	}
}

func BuildCreateTemplateComponents(textBody string, buttons []string) []MessageTemplateCreateComponent {
	return buildCreateTemplateComponents(textBody, buttons)
}

func buildCreateTemplateComponents(textBody string, buttons []string) []MessageTemplateCreateComponent {
	components := []MessageTemplateCreateComponent{{Type: "BODY", Text: textBody}}
	if len(buttons) == 0 {
		return components
	}
	quickReplyButtons := make([]MessageTemplateButton, len(buttons))
	for i, label := range buttons {
		quickReplyButtons[i] = MessageTemplateButton{Type: "QUICK_REPLY", Text: strings.TrimSpace(label)}
	}
	components = append(components, MessageTemplateCreateComponent{Type: "BUTTONS", Buttons: quickReplyButtons})
	return components
}

func (p *MetaProvider) CreateTemplate(
	ctx context.Context,
	accessToken, wabaID, name, category, textBody string,
	buttons []string,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	wabaID = strings.TrimSpace(wabaID)
	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if wabaID == "" {
		return "", fmt.Errorf("waba id is required")
	}

	name = strings.TrimSpace(name)
	category = strings.ToUpper(strings.TrimSpace(category))
	textBody = strings.TrimSpace(textBody)
	if name == "" {
		return "", fmt.Errorf("template name is required")
	}
	if category == "" {
		category = "UTILITY"
	}
	if textBody == "" {
		return "", fmt.Errorf("template body text is required")
	}

	payload := MessageTemplateCreateRequest{
		Name: name, Category: category, AllowCategoryChange: true,
		Language: p.languageCode, Components: buildCreateTemplateComponents(textBody, buttons),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal create template payload: %w", err)
	}

	respBody, err := p.doMetaRequest(ctx, accessToken, http.MethodPost, p.messageTemplatesURL(wabaID), body)
	if err != nil {
		return "", err
	}

	var created createMessageTemplateResponse
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", fmt.Errorf("decode create template response: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("meta api returned empty template id")
	}
	return created.ID, nil
}

func (p *MetaProvider) GetTemplatesStatus(
	ctx context.Context,
	accessToken, wabaID string,
) ([]MetaTemplateResponse, error) {
	accessToken = strings.TrimSpace(accessToken)
	wabaID = strings.TrimSpace(wabaID)
	if accessToken == "" {
		return nil, fmt.Errorf("access token is required")
	}
	if wabaID == "" {
		return nil, fmt.Errorf("waba id is required")
	}

	nextURL := p.messageTemplatesURL(wabaID) +
		"?fields=" + url.QueryEscape("id,name,status,category,language,components,quality_score") +
		"&limit=100"

	templates := make([]MetaTemplateResponse, 0)
	for nextURL != "" {
		respBody, err := p.doMetaRequest(ctx, accessToken, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}

		var listed listMessageTemplatesResponse
		if err := json.Unmarshal(respBody, &listed); err != nil {
			return nil, fmt.Errorf("decode list templates response: %w", err)
		}

		for _, item := range listed.Data {
			templates = append(templates, MetaTemplateResponse{
				ID:           item.ID,
				Name:         item.Name,
				Status:       item.Status,
				Category:     item.Category,
				Language:     item.Language,
				Components:   item.Components,
				QualityScore: parseQualityScore(item.QualityScore),
			})
		}

		nextURL = strings.TrimSpace(listed.Paging.Next)
		if nextURL != "" && !strings.HasPrefix(nextURL, "https://") && !strings.HasPrefix(nextURL, "http://") {
			nextURL = ""
		}
	}
	return templates, nil
}

func parseQualityScore(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var asObj struct {
		Score string `json:"score"`
	}
	if err := json.Unmarshal(raw, &asObj); err == nil {
		return strings.TrimSpace(asObj.Score)
	}
	return strings.TrimSpace(string(raw))
}

// SendPlainTemplate envia template sem componentes dinâmicos (ex.: hello_world).
func (p *MetaProvider) SendPlainTemplate(
	ctx context.Context,
	accessToken, phoneNumberID, to, templateName, languageCode string,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	to = strings.TrimSpace(to)
	templateName = strings.TrimSpace(templateName)
	languageCode = strings.TrimSpace(languageCode)
	if languageCode == "" {
		languageCode = p.languageCode
	}
	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return "", fmt.Errorf("phone number id is required")
	}
	if to == "" {
		return "", fmt.Errorf("recipient phone number is required")
	}
	if templateName == "" {
		return "", fmt.Errorf("template name is required")
	}

	payload := SendMessageRequest{
		MessagingProduct: "whatsapp",
		To:               to,
		Type:             "template",
		Template: Template{
			Name:     templateName,
			Language: TemplateLanguage{Code: languageCode},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal template payload: %w", err)
	}

	respBody, err := p.doMetaRequest(ctx, accessToken, http.MethodPost, p.messagesURL(phoneNumberID), body)
	if err != nil {
		return "", err
	}

	var sent sendMessageResponse
	if err := json.Unmarshal(respBody, &sent); err != nil {
		return "", fmt.Errorf("decode send message response: %w", err)
	}
	if len(sent.Messages) == 0 || strings.TrimSpace(sent.Messages[0].ID) == "" {
		return "", fmt.Errorf("meta api returned empty message id")
	}
	return strings.TrimSpace(sent.Messages[0].ID), nil
}

// SendAppointmentTemplate envia via o chip do tenant e retorna o wamid da Meta.
func (p *MetaProvider) SendAppointmentTemplate(
	ctx context.Context,
	accessToken, phoneNumberID, to, templateName string,
	vars []string,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	to = strings.TrimSpace(to)
	templateName = strings.TrimSpace(templateName)
	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return "", fmt.Errorf("phone number id is required")
	}
	if to == "" {
		return "", fmt.Errorf("recipient phone number is required")
	}
	if templateName == "" {
		return "", fmt.Errorf("template name is required")
	}

	payload := SendMessageRequest{
		MessagingProduct: "whatsapp",
		To:               to,
		Type:             "template",
		Template: Template{
			Name:     templateName,
			Language: TemplateLanguage{Code: p.languageCode},
			Components: []TemplateComponent{
				BodyComponent(vars),
				QuickReplyButton(0, ButtonPayloadConfirm),
				QuickReplyButton(1, ButtonPayloadReschedule),
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal template payload: %w", err)
	}

	respBody, err := p.doMetaRequest(ctx, accessToken, http.MethodPost, p.messagesURL(phoneNumberID), body)
	if err != nil {
		return "", err
	}

	var sent sendMessageResponse
	if err := json.Unmarshal(respBody, &sent); err != nil {
		return "", fmt.Errorf("decode send message response: %w", err)
	}
	if len(sent.Messages) == 0 || strings.TrimSpace(sent.Messages[0].ID) == "" {
		return "", fmt.Errorf("meta api returned empty message id")
	}
	return strings.TrimSpace(sent.Messages[0].ID), nil
}

// SendTextMessage envia mensagem de texto livre com o token do tenant.
func (p *MetaProvider) SendTextMessage(ctx context.Context, accessToken, phoneNumberID, to, body string) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	to = strings.TrimSpace(to)
	body = strings.TrimSpace(body)
	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return "", fmt.Errorf("phone number id is required")
	}
	if to == "" {
		return "", fmt.Errorf("recipient phone number is required")
	}
	if body == "" {
		return "", fmt.Errorf("message body is required")
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text": map[string]any{
			"preview_url": false,
			"body":        body,
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal text payload: %w", err)
	}

	respBody, err := p.doMetaRequest(ctx, accessToken, http.MethodPost, p.messagesURL(phoneNumberID), bodyBytes)
	if err != nil {
		return "", err
	}
	var sent sendMessageResponse
	if err := json.Unmarshal(respBody, &sent); err != nil {
		return "", fmt.Errorf("decode send message response: %w", err)
	}
	if len(sent.Messages) == 0 || strings.TrimSpace(sent.Messages[0].ID) == "" {
		return "", fmt.Errorf("meta api returned empty message id")
	}
	return strings.TrimSpace(sent.Messages[0].ID), nil
}

// SendUtilityTemplate envia template utility com token do tenant.
func (p *MetaProvider) SendUtilityTemplate(
	ctx context.Context,
	accessToken, phoneNumberID, to, templateName string,
	bodyParams []string,
) error {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	to = strings.TrimSpace(to)
	templateName = strings.TrimSpace(templateName)
	if accessToken == "" {
		return fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return fmt.Errorf("phone number id is required")
	}
	if to == "" {
		return fmt.Errorf("recipient phone number is required")
	}
	if templateName == "" {
		return fmt.Errorf("template name is required")
	}

	payload := SendMessageRequest{
		MessagingProduct: "whatsapp",
		To:               to,
		Type:             "template",
		Template: Template{
			Name:       templateName,
			Language:   TemplateLanguage{Code: p.languageCode},
			Components: []TemplateComponent{BodyComponent(bodyParams)},
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal template payload: %w", err)
	}

	_, err = p.doMetaRequest(ctx, accessToken, http.MethodPost, p.messagesURL(phoneNumberID), bodyBytes)
	return err
}

func (p *MetaProvider) messagesURL(phoneNumberID string) string {
	return fmt.Sprintf("%s/%s/%s/messages", graphAPIBaseURL, p.apiVersion, phoneNumberID)
}

func (p *MetaProvider) messageTemplatesURL(wabaID string) string {
	return fmt.Sprintf("%s/%s/%s/message_templates", graphAPIBaseURL, p.apiVersion, wabaID)
}

func (p *MetaProvider) doMetaRequest(ctx context.Context, accessToken, method, url string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, parseMetaHTTPError(resp.StatusCode, respBody)
	}
	return respBody, nil
}

func parseMetaHTTPError(statusCode int, body []byte) error {
	var metaErr metaErrorResponse
	if err := json.Unmarshal(body, &metaErr); err == nil && metaErr.Error.Message != "" {
		return &MetaAPIError{
			Status:  statusCode,
			Code:    metaErr.Error.Code,
			Message: metaErr.Error.Message,
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(statusCode)
	}
	return &MetaAPIError{Status: statusCode, Message: msg}
}
