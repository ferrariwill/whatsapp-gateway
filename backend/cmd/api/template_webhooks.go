package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strconv"
	"strings"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/templateutil"
)

type templateManagementValue struct {
	Event                   string          `json:"event"`
	MessageTemplateID       json.RawMessage `json:"message_template_id"`
	MessageTemplateName     string          `json:"message_template_name"`
	MessageTemplateLanguage string          `json:"message_template_language"`
	MessageTemplateCategory string          `json:"message_template_category"`
	Reason                  string          `json:"reason"`
	PreviousQualityScore    json.RawMessage `json:"previous_quality_score"`
	NewQualityScore         json.RawMessage `json:"new_quality_score"`
	Components              json.RawMessage `json:"components"`
}

func isTemplateManagementField(field string) bool {
	switch strings.TrimSpace(field) {
	case "message_template_status_update",
		"message_template_quality_update",
		"message_template_components_update":
		return true
	default:
		return false
	}
}

func payloadHasTemplateManagement(payload unifiedMetaWebhookPayload) bool {
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if isTemplateManagementField(change.Field) {
				return true
			}
		}
	}
	return false
}

type extendedEntry struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Changes []struct {
		Field string          `json:"field"`
		Value json.RawMessage `json:"value"`
	} `json:"changes"`
}

type extendedMetaWebhookPayload struct {
	Object string          `json:"object"`
	Entry  []extendedEntry `json:"entry"`
}

func (s *server) processTemplateManagementRaw(ctx context.Context, body []byte) {
	var payload extendedMetaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("template webhook: invalid extended payload: %v", err)
		return
	}
	for _, entry := range payload.Entry {
		wabaID := strings.TrimSpace(entry.ID)
		for _, change := range entry.Changes {
			if !isTemplateManagementField(change.Field) {
				continue
			}
			var value templateManagementValue
			if err := json.Unmarshal(change.Value, &value); err != nil {
				log.Printf("template webhook decode %s: %v", change.Field, err)
				continue
			}
			s.applyTemplateManagementChange(ctx, wabaID, change.Field, value, entry.Time)
		}
	}
}

func (s *server) applyTemplateManagementChange(
	ctx context.Context,
	wabaID, field string,
	value templateManagementValue,
	eventTime int64,
) {
	metaID := decodeFlexibleID(value.MessageTemplateID)
	name := strings.TrimSpace(value.MessageTemplateName)
	language := strings.TrimSpace(value.MessageTemplateLanguage)
	status := templateutil.NormalizeTemplateStatus(value.Event)
	quality := firstNonEmpty(
		parseQualityRaw(value.NewQualityScore),
		parseQualityRaw(value.PreviousQualityScore),
	)

	eventKey := buildTemplateEventKey(field, metaID, name, language, status, quality, eventTime)
	inserted, err := s.repo.TryInsertWebhookDedupe(ctx, eventKey)
	if err != nil {
		log.Printf("template webhook dedupe: %v", err)
		return
	}
	if !inserted {
		return
	}

	conns, err := s.repo.FindConnectionsByWabaID(ctx, wabaID)
	if err != nil {
		log.Printf("template webhook find connections waba=%s: %v", wabaID, err)
		return
	}
	if len(conns) == 0 {
		log.Printf("template webhook: unknown waba_id %s (field=%s)", wabaID, field)
		return
	}

	for _, conn := range conns {
		switch field {
		case "message_template_status_update":
			if status == "" {
				continue
			}
			updated := false
			if metaID != "" {
				n, err := s.repo.UpdateTemplateStatusByMetaID(ctx, conn.SystemID, metaID, status, quality)
				if err != nil {
					log.Printf("template status by meta_id: %v", err)
				} else if n > 0 {
					updated = true
				}
			}
			if !updated && name != "" && language != "" {
				if _, err := s.repo.UpdateTemplateStatusByNameLang(
					ctx, conn.SystemID, conn.TenantID, name, language, status, nil,
				); err != nil {
					log.Printf("template status by name/lang %s/%s: %v", conn.TenantID, name, err)
				}
			}
		case "message_template_quality_update":
			if name == "" || language == "" || quality == "" {
				continue
			}
			if _, err := s.repo.UpdateTemplateQualityByNameLang(
				ctx, conn.SystemID, conn.TenantID, name, language, quality,
			); err != nil {
				log.Printf("template quality update %s/%s: %v", conn.TenantID, name, err)
			}
		case "message_template_components_update":
			components := value.Components
			if len(components) == 0 {
				components = json.RawMessage("[]")
			}
			if status == "" {
				status = model.TemplateStatusPending
			}
			if name == "" || language == "" {
				continue
			}
			if _, err := s.repo.UpdateTemplateStatusByNameLang(
				ctx, conn.SystemID, conn.TenantID, name, language, status, []byte(components),
			); err != nil {
				log.Printf("template components update %s/%s: %v", conn.TenantID, name, err)
			}
		}
	}
}

func buildTemplateEventKey(field, metaID, name, language, status, quality string, eventTime int64) string {
	base := strings.Join([]string{
		field, metaID, name, language, status, quality, strconv.FormatInt(eventTime, 10),
	}, "|")
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:])
}

func decodeFlexibleID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return asNumber.String()
	}
	return strings.Trim(string(raw), `"`)
}

func parseQualityRaw(raw json.RawMessage) string {
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
	return ""
}
