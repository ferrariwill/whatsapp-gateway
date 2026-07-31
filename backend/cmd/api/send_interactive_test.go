package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateInteractiveRequestTable(t *testing.T) {
	validButton := func() sendInteractiveRequest {
		return sendInteractiveRequest{
			Type:     "button",
			BodyText: "Confirma?",
			Buttons: []interactiveButtonReq{
				{ID: "CONFIRM", Title: "Confirmar"},
				{ID: "CANCEL", Title: "Cancelar"},
			},
		}
	}
	validList := func() sendInteractiveRequest {
		return sendInteractiveRequest{
			Type:       "list",
			BodyText:   "Escolha",
			ListButton: "Ver horários",
			Sections: []interactiveListSectionReq{{
				Title: "Manhã",
				Rows:  []interactiveListRowReq{{ID: "slot_0900", Title: "09:00", Description: "Dr. Silva"}},
			}},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*sendInteractiveRequest)
		wantErr string
	}{
		{name: "ok button", mutate: func(r *sendInteractiveRequest) {}, wantErr: ""},
		{name: "ok list", mutate: func(r *sendInteractiveRequest) { *r = validList() }, wantErr: ""},
		{
			name: "4 buttons",
			mutate: func(r *sendInteractiveRequest) {
				r.Buttons = []interactiveButtonReq{
					{ID: "a", Title: "A"}, {ID: "b", Title: "B"},
					{ID: "c", Title: "C"}, {ID: "d", Title: "D"},
				}
			},
			wantErr: "1–3 buttons",
		},
		{
			name: "title too long",
			mutate: func(r *sendInteractiveRequest) {
				r.Buttons[0].Title = strings.Repeat("x", 21)
			},
			wantErr: "≤20",
		},
		{
			name: "duplicate button ids",
			mutate: func(r *sendInteractiveRequest) {
				r.Buttons[1].ID = "CONFIRM"
			},
			wantErr: "unique",
		},
		{
			name: "body empty",
			mutate: func(r *sendInteractiveRequest) {
				r.BodyText = "   "
			},
			wantErr: "body_text",
		},
		{
			name: "list too many rows",
			mutate: func(r *sendInteractiveRequest) {
				*r = validList()
				rows := make([]interactiveListRowReq, 0, 11)
				for i := 0; i < 11; i++ {
					rows = append(rows, interactiveListRowReq{
						ID: fmt.Sprintf("r%d", i), Title: "T",
					})
				}
				r.Sections = []interactiveListSectionReq{{Rows: rows}}
			},
			wantErr: "10 rows",
		},
		{
			name: "list_button too long",
			mutate: func(r *sendInteractiveRequest) {
				*r = validList()
				r.ListButton = strings.Repeat("y", 21)
			},
			wantErr: "list_button",
		},
		{
			name: "bad type",
			mutate: func(r *sendInteractiveRequest) {
				r.Type = "cta"
			},
			wantErr: "button or list",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validButton()
			tc.mutate(&req)
			err := validateInteractiveRequest(&req)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !isInteractiveValidationError(err) {
				t.Fatalf("want interactiveValidationError, got %T", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%q want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseInboundListReplyAndContext(t *testing.T) {
	msg := metaInboundMessage{
		ID:   "wamid.reply",
		From: "5511999999999",
		Type: "interactive",
		Context: &struct {
			ID string `json:"id"`
		}{ID: "wamid.outbound"},
		Interactive: &struct {
			Type        string `json:"type"`
			ButtonReply *struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"button_reply"`
			ListReply *struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"list_reply"`
		}{
			Type: "list_reply",
			ListReply: &struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}{ID: "slot_0900", Title: "09:00"},
		},
	}
	got, ok := parseInboundMessage(msg)
	if !ok {
		t.Fatal("expected ok")
	}
	if got.eventType != "list_reply" {
		t.Fatalf("eventType=%q", got.eventType)
	}
	if got.replyID != "slot_0900" || got.replyTitle != "09:00" {
		t.Fatalf("reply=%q/%q", got.replyID, got.replyTitle)
	}
	if got.contextMessageID != "wamid.outbound" {
		t.Fatalf("context=%q", got.contextMessageID)
	}
	if got.text != "slot_0900" {
		t.Fatalf("text=%q", got.text)
	}

	raw, err := marshalInboundEventPayload(got)
	if err != nil {
		t.Fatal(err)
	}
	round := inboundEventFromPayloadJSON(raw, inboundEvent{})
	if round.replyID != got.replyID || round.contextMessageID != got.contextMessageID {
		t.Fatalf("roundtrip reply/context lost: %+v", round)
	}
}

func TestParseInboundButtonReplyFillsReplyFields(t *testing.T) {
	msg := metaInboundMessage{
		ID:   "wamid.btn",
		From: "5511888777666",
		Type: "interactive",
		Interactive: &struct {
			Type        string `json:"type"`
			ButtonReply *struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"button_reply"`
			ListReply *struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"list_reply"`
		}{
			Type: "button_reply",
			ButtonReply: &struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}{ID: "CONFIRM", Title: "Confirmar"},
		},
	}
	got, ok := parseInboundMessage(msg)
	if !ok {
		t.Fatal("expected ok")
	}
	if got.eventType != "button_reply" || got.replyID != "CONFIRM" || got.replyTitle != "Confirmar" {
		t.Fatalf("got event=%q reply=%q/%q", got.eventType, got.replyID, got.replyTitle)
	}
	if got.action != "" {
		t.Fatalf("generic CONFIRM id should leave action empty, got %q", got.action)
	}

	var saas saasWebhookPayload
	applyInboundEventToSaaSPayload(&saas, got)
	if saas.From != got.from || saas.ReplyID != "CONFIRM" || saas.ReplyTitle != "Confirmar" {
		t.Fatalf("saas payload incomplete: %+v", saas)
	}
}
