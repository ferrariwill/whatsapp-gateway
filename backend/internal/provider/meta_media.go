package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

// MediaMessageType is the WhatsApp Cloud API message type for outbound media.
type MediaMessageType string

const (
	MediaTypeImage    MediaMessageType = "image"
	MediaTypeDocument MediaMessageType = "document"
)

// UploadMediaResult is the Graph media id returned by POST /{phone-number-id}/media.
type UploadMediaResult struct {
	ID string `json:"id"`
}

// UploadMedia envia binário local para a Meta e devolve o media_id.
func (p *MetaProvider) UploadMedia(
	ctx context.Context,
	accessToken, phoneNumberID, mimeType, filename string,
	data []byte,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	mimeType = strings.TrimSpace(mimeType)
	filename = strings.TrimSpace(filename)
	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return "", fmt.Errorf("phone number id is required")
	}
	if mimeType == "" {
		return "", fmt.Errorf("mime type is required")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("media body is required")
	}
	if filename == "" {
		filename = "file"
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("messaging_product", "whatsapp"); err != nil {
		return "", fmt.Errorf("write messaging_product: %w", err)
	}
	if err := writer.WriteField("type", mimeType); err != nil {
		return "", fmt.Errorf("write type: %w", err)
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartFilename(filename)))
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write file part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.mediaURL(phoneNumberID), &buf)
	if err != nil {
		return "", fmt.Errorf("create media upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute media upload: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read media upload response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", parseMetaHTTPError(resp.StatusCode, respBody)
	}

	var uploaded UploadMediaResult
	if err := json.Unmarshal(respBody, &uploaded); err != nil {
		return "", fmt.Errorf("decode media upload response: %w", err)
	}
	id := strings.TrimSpace(uploaded.ID)
	if id == "" {
		return "", fmt.Errorf("meta api returned empty media id")
	}
	return id, nil
}

// SendMediaMessage envia image/document com media_id ou link HTTPS.
func (p *MetaProvider) SendMediaMessage(
	ctx context.Context,
	accessToken, phoneNumberID, to string,
	mediaType MediaMessageType,
	mediaID, link, caption, filename string,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	to = strings.TrimSpace(to)
	mediaID = strings.TrimSpace(mediaID)
	link = strings.TrimSpace(link)
	caption = strings.TrimSpace(caption)
	filename = strings.TrimSpace(filename)

	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return "", fmt.Errorf("phone number id is required")
	}
	if to == "" {
		return "", fmt.Errorf("recipient phone number is required")
	}
	if mediaType != MediaTypeImage && mediaType != MediaTypeDocument {
		return "", fmt.Errorf("unsupported media type %q", mediaType)
	}
	if mediaID == "" && link == "" {
		return "", fmt.Errorf("media_id or link is required")
	}
	if mediaID != "" && link != "" {
		return "", fmt.Errorf("provide either media_id or link, not both")
	}

	mediaObj := map[string]any{}
	if mediaID != "" {
		mediaObj["id"] = mediaID
	} else {
		mediaObj["link"] = link
	}
	if caption != "" {
		mediaObj["caption"] = caption
	}
	if mediaType == MediaTypeDocument && filename != "" {
		mediaObj["filename"] = filename
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              string(mediaType),
		string(mediaType):   mediaObj,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal media payload: %w", err)
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

func (p *MetaProvider) mediaURL(phoneNumberID string) string {
	return fmt.Sprintf("%s/%s/%s/media", graphAPIBaseURL, p.apiVersion, phoneNumberID)
}

func escapeMultipartFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, `_`)
	name = strings.ReplaceAll(name, `"`, `_`)
	return name
}
