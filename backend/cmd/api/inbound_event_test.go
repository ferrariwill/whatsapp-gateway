package main

import (
	"errors"
	"net/http"
	"testing"
)

func TestParseInboundMessageTypedKinds(t *testing.T) {
	cases := []struct {
		name      string
		msg       metaInboundMessage
		wantType  string
		wantMedia string
		auditOnly bool
	}{
		{
			name: "image",
			msg: metaInboundMessage{
				ID: "m1", From: "5511", Type: "image",
				Image: &metaMediaObject{ID: "img-1", MimeType: "image/jpeg", Caption: "oi"},
			},
			wantType: "image_message", wantMedia: "img-1",
		},
		{
			name: "location",
			msg: metaInboundMessage{
				ID: "m2", From: "5511", Type: "location",
				Location: &struct {
					Latitude  float64 `json:"latitude"`
					Longitude float64 `json:"longitude"`
					Name      string  `json:"name"`
					Address   string  `json:"address"`
				}{Latitude: -23, Longitude: -46, Name: "SP"},
			},
			wantType: "location_message",
		},
		{
			name: "reaction",
			msg: metaInboundMessage{
				ID: "m3", From: "5511", Type: "reaction",
				Reaction: &struct {
					MessageID string `json:"message_id"`
					Emoji     string `json:"emoji"`
				}{MessageID: "orig", Emoji: "👍"},
			},
			wantType: "reaction_message",
		},
		{
			name:      "unknown",
			msg:       metaInboundMessage{ID: "m4", From: "5511", Type: "sticker"},
			wantType:  "unknown_message",
			auditOnly: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseInboundMessage(tc.msg)
			if !ok {
				t.Fatal("expected ok")
			}
			if got.eventType != tc.wantType {
				t.Fatalf("eventType=%q want %q", got.eventType, tc.wantType)
			}
			if got.auditOnly != tc.auditOnly {
				t.Fatalf("auditOnly=%v want %v", got.auditOnly, tc.auditOnly)
			}
			if tc.wantMedia != "" && (got.media == nil || got.media.ID != tc.wantMedia) {
				t.Fatalf("media=%+v", got.media)
			}
			raw, err := marshalInboundEventPayload(got)
			if err != nil || len(raw) == 0 {
				t.Fatalf("marshal: %v len=%d", err, len(raw))
			}
			round := inboundEventFromPayloadJSON(raw, inboundEvent{})
			if round.eventType != got.eventType {
				t.Fatalf("roundtrip type %q != %q", round.eventType, got.eventType)
			}
		})
	}
}

func TestRelayErrorClassification(t *testing.T) {
	if !newRelayHTTPError(http.StatusBadRequest, "x").Permanent() {
		t.Fatal("400 should be permanent")
	}
	if newRelayHTTPError(http.StatusTooManyRequests, "x").Permanent() {
		t.Fatal("429 should be transient")
	}
	if newRelayHTTPError(http.StatusInternalServerError, "x").Permanent() {
		t.Fatal("500 should be transient")
	}
	if !isTransientRelayError(newRelayTransportError(errors.New("timeout"))) {
		t.Fatal("transport should be transient")
	}
	if relayFailureReason(newRelayHTTPError(404, "missing")) != failureReasonPermanent {
		t.Fatal("404 reason")
	}
}
