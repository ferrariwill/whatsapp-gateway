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

// ImageSendOpts sends a WhatsApp image; exactly one of Link or MediaID is required.
type ImageSendOpts struct {
	Link    string
	MediaID string
	Caption string
}

// DocumentSendOpts sends a WhatsApp document; exactly one of Link or MediaID is required.
type DocumentSendOpts struct {
	Link     string
	MediaID  string
	Caption  string
	Filename string
}

type uploadMediaResponse struct {
	ID string `json:"id"`
}

// UploadMedia posts a binary to Graph POST /{phone-number-id}/media.
func (p *MetaProvider) UploadMedia(
	ctx context.Context,
	accessToken, phoneNumberID, filename, mimeType string,
	r io.Reader,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	filename = strings.TrimSpace(filename)
	mimeType = strings.TrimSpace(mimeType)
	if accessToken == "" {
		return "", fmt.Errorf("access token is required")
	}
	if phoneNumberID == "" {
		return "", fmt.Errorf("phone number id is required")
	}
	if mimeType == "" {
		return "", fmt.Errorf("mime type is required")
	}
	if r == nil {
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
	if _, err := io.Copy(part, r); err != nil {
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

	var uploaded uploadMediaResponse
	if err := json.Unmarshal(respBody, &uploaded); err != nil {
		return "", fmt.Errorf("decode media upload response: %w", err)
	}
	id := strings.TrimSpace(uploaded.ID)
	if id == "" {
		return "", fmt.Errorf("meta api returned empty media id")
	}
	return id, nil
}

// SendImageMessage sends type=image with link or media id.
func (p *MetaProvider) SendImageMessage(
	ctx context.Context,
	accessToken, phoneNumberID, to string,
	opts ImageSendOpts,
) (string, error) {
	return p.sendMediaMessage(ctx, accessToken, phoneNumberID, to, "image", opts.Link, opts.MediaID, opts.Caption, "")
}

// SendDocumentMessage sends type=document with link or media id.
func (p *MetaProvider) SendDocumentMessage(
	ctx context.Context,
	accessToken, phoneNumberID, to string,
	opts DocumentSendOpts,
) (string, error) {
	return p.sendMediaMessage(ctx, accessToken, phoneNumberID, to, "document", opts.Link, opts.MediaID, opts.Caption, opts.Filename)
}

func (p *MetaProvider) sendMediaMessage(
	ctx context.Context,
	accessToken, phoneNumberID, to, mediaType, link, mediaID, caption, filename string,
) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	to = strings.TrimSpace(to)
	link = strings.TrimSpace(link)
	mediaID = strings.TrimSpace(mediaID)
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
	if mediaType == "document" && filename != "" {
		mediaObj["filename"] = filename
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              mediaType,
		mediaType:           mediaObj,
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
