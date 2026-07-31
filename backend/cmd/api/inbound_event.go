package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/whatsappgetway/gateway/internal/provider"
)

// inboundMedia carrega IDs/metadados de mídia tipada (image/audio/document).
type inboundMedia struct {
	ID       string `json:"id,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Caption  string `json:"caption,omitempty"`
	Filename string `json:"filename,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Voice    bool   `json:"voice,omitempty"`
}

type inboundLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Name      string  `json:"name,omitempty"`
	Address   string  `json:"address,omitempty"`
}

type inboundReaction struct {
	MessageID string `json:"message_id,omitempty"`
	Emoji     string `json:"emoji,omitempty"`
}

type inboundEvent struct {
	id        string
	from      string
	text      string
	eventType string
	action    string
	media     *inboundMedia
	location  *inboundLocation
	reaction  *inboundReaction
	rawType   string
	auditOnly bool // tipos desconhecidos: auditados, não repassados
}

func (e inboundEvent) displayText() string {
	if strings.TrimSpace(e.text) != "" {
		return e.text
	}
	if e.media != nil && e.media.Caption != "" {
		return e.media.Caption
	}
	if e.media != nil && e.media.ID != "" {
		return e.media.ID
	}
	if e.location != nil {
		return fmt.Sprintf("%f,%f", e.location.Latitude, e.location.Longitude)
	}
	if e.reaction != nil {
		return e.reaction.Emoji
	}
	if e.rawType != "" {
		return e.rawType
	}
	return e.eventType
}

// inboundEventPayload é a forma normalizada persistida em message_logs.inbound_payload
// para que o replay/DLQ preserve mídia/localização/reação.
type inboundEventPayload struct {
	ID        string           `json:"id"`
	From      string           `json:"from"`
	Text      string           `json:"text,omitempty"`
	EventType string           `json:"event_type"`
	Action    string           `json:"action,omitempty"`
	Media     *inboundMedia    `json:"media,omitempty"`
	Location  *inboundLocation `json:"location,omitempty"`
	Reaction  *inboundReaction `json:"reaction,omitempty"`
	RawType   string           `json:"raw_type,omitempty"`
	AuditOnly bool             `json:"audit_only,omitempty"`
}

func marshalInboundEventPayload(event inboundEvent) ([]byte, error) {
	return json.Marshal(inboundEventPayload{
		ID:        event.id,
		From:      event.from,
		Text:      event.text,
		EventType: event.eventType,
		Action:    event.action,
		Media:     event.media,
		Location:  event.location,
		Reaction:  event.reaction,
		RawType:   event.rawType,
		AuditOnly: event.auditOnly,
	})
}

func inboundEventFromPayloadJSON(raw []byte, fallback inboundEvent) inboundEvent {
	if len(raw) == 0 {
		return fallback
	}
	var p inboundEventPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		log.Printf("inbound_payload unmarshal: %v", err)
		return fallback
	}
	return inboundEvent{
		id:        firstNonEmpty(p.ID, fallback.id),
		from:      firstNonEmpty(p.From, fallback.from),
		text:      firstNonEmpty(p.Text, fallback.text),
		eventType: firstNonEmpty(p.EventType, fallback.eventType),
		action:    firstNonEmpty(p.Action, fallback.action),
		media:     p.Media,
		location:  p.Location,
		reaction:  p.Reaction,
		rawType:   p.RawType,
		auditOnly: p.AuditOnly,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

type metaMediaObject struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Voice    bool   `json:"voice"`
}

type metaInboundMessage struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Type string `json:"type"`
	Text *struct {
		Body string `json:"body"`
	} `json:"text"`
	Button *struct {
		Payload string `json:"payload"`
		Text    string `json:"text"`
	} `json:"button"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply"`
	} `json:"interactive"`
	Image    *metaMediaObject `json:"image"`
	Audio    *metaMediaObject `json:"audio"`
	Document *metaMediaObject `json:"document"`
	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
	} `json:"location"`
	Reaction *struct {
		MessageID string `json:"message_id"`
		Emoji     string `json:"emoji"`
	} `json:"reaction"`
}

func parseInboundMessage(message metaInboundMessage) (inboundEvent, bool) {
	from := strings.TrimSpace(message.From)
	if from == "" {
		return inboundEvent{}, false
	}
	messageID := strings.TrimSpace(message.ID)
	rawType := strings.TrimSpace(message.Type)

	switch rawType {
	case "text":
		if message.Text == nil || strings.TrimSpace(message.Text.Body) == "" {
			return inboundEvent{}, false
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      strings.TrimSpace(message.Text.Body),
			eventType: "text_message",
			rawType:   rawType,
		}, true

	case "button":
		if message.Button == nil {
			return inboundEvent{}, false
		}
		payload := strings.TrimSpace(message.Button.Payload)
		if payload == "" {
			payload = strings.TrimSpace(message.Button.Text)
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      payload,
			eventType: "button_reply",
			action:    mapButtonAction(payload),
			rawType:   rawType,
		}, true

	case "interactive":
		if message.Interactive == nil || message.Interactive.ButtonReply == nil {
			return inboundEvent{}, false
		}
		payload := strings.TrimSpace(message.Interactive.ButtonReply.ID)
		if payload == "" {
			payload = strings.TrimSpace(message.Interactive.ButtonReply.Title)
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      payload,
			eventType: "button_reply",
			action:    mapButtonAction(payload),
			rawType:   rawType,
		}, true

	case "image":
		media := mediaFromMeta(message.Image)
		if media == nil {
			return inboundEvent{}, false
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      media.Caption,
			eventType: "image_message",
			media:     media,
			rawType:   rawType,
		}, true

	case "audio":
		media := mediaFromMeta(message.Audio)
		if media == nil {
			return inboundEvent{}, false
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			eventType: "audio_message",
			media:     media,
			rawType:   rawType,
		}, true

	case "document":
		media := mediaFromMeta(message.Document)
		if media == nil {
			return inboundEvent{}, false
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      media.Caption,
			eventType: "document_message",
			media:     media,
			rawType:   rawType,
		}, true

	case "location":
		if message.Location == nil {
			return inboundEvent{}, false
		}
		loc := &inboundLocation{
			Latitude:  message.Location.Latitude,
			Longitude: message.Location.Longitude,
			Name:      strings.TrimSpace(message.Location.Name),
			Address:   strings.TrimSpace(message.Location.Address),
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      loc.Name,
			eventType: "location_message",
			location:  loc,
			rawType:   rawType,
		}, true

	case "reaction":
		if message.Reaction == nil {
			return inboundEvent{}, false
		}
		re := &inboundReaction{
			MessageID: strings.TrimSpace(message.Reaction.MessageID),
			Emoji:     strings.TrimSpace(message.Reaction.Emoji),
		}
		return inboundEvent{
			id:        messageID,
			from:      from,
			text:      re.Emoji,
			eventType: "reaction_message",
			reaction:  re,
			rawType:   rawType,
		}, true
	}

	// Desconhecido: auditar/logar — nunca descartar em silêncio.
	log.Printf("inbound unknown message type=%q meta_message_id=%s from=%s — auditing", rawType, messageID, from)
	return inboundEvent{
		id:        messageID,
		from:      from,
		text:      rawType,
		eventType: "unknown_message",
		rawType:   rawType,
		auditOnly: true,
	}, true
}

func mediaFromMeta(src *metaMediaObject) *inboundMedia {
	if src == nil {
		return nil
	}
	id := strings.TrimSpace(src.ID)
	if id == "" {
		return nil
	}
	return &inboundMedia{
		ID:       id,
		MimeType: strings.TrimSpace(src.MimeType),
		Caption:  strings.TrimSpace(src.Caption),
		Filename: strings.TrimSpace(src.Filename),
		SHA256:   strings.TrimSpace(src.SHA256),
		Voice:    src.Voice,
	}
}

func mapButtonAction(payload string) string {
	switch payload {
	case provider.ButtonPayloadConfirm:
		return "CONFIRM"
	case provider.ButtonPayloadReschedule:
		return "CANCEL"
	default:
		return ""
	}
}

func applyInboundEventToSaaSPayload(base *saasWebhookPayload, event inboundEvent) {
	base.MetaMessageID = event.id
	base.PhoneNumber = event.from
	base.Text = event.text
	base.EventType = event.eventType
	base.Action = event.action
	base.Media = event.media
	base.Location = event.location
	base.Reaction = event.reaction
}

func applyInboundEventToLegacyPayload(base *outboundWebhookPayload, event inboundEvent) {
	base.MetaMessageID = event.id
	base.PhoneNumber = event.from
	base.Text = event.text
	base.EventType = event.eventType
	base.Action = event.action
	base.Media = event.media
	base.Location = event.location
	base.Reaction = event.reaction
}
