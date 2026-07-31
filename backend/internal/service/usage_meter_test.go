package service

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyOutbound(t *testing.T) {
	text, tmpl, media := ClassifyOutbound("hello_world", "")
	if text != 0 || tmpl != 1 || media != 0 {
		t.Errorf("template: got %d/%d/%d", text, tmpl, media)
	}
	text, tmpl, media = ClassifyOutbound("", "image")
	if text != 0 || tmpl != 0 || media != 1 {
		t.Errorf("media: got %d/%d/%d", text, tmpl, media)
	}
	text, tmpl, media = ClassifyOutbound("", "text")
	if text != 1 || tmpl != 0 || media != 0 {
		t.Errorf("text: got %d/%d/%d", text, tmpl, media)
	}
}

func TestClassifySendError(t *testing.T) {
	e4, e5 := ClassifySendError(errors.New("meta api error (status 400, code 100): bad request"))
	if e4 != 1 || e5 != 0 {
		t.Errorf("4xx: got %d/%d", e4, e5)
	}
	e4, e5 = ClassifySendError(errors.New("meta api error (status 503, code 2): unavailable"))
	if e4 != 0 || e5 != 1 {
		t.Errorf("5xx: got %d/%d", e4, e5)
	}
	e4, e5 = ClassifySendError(errors.New("Post \"https://graph.facebook.com\": context deadline exceeded"))
	if e4 != 0 || e5 != 1 {
		t.Errorf("timeout: got %d/%d", e4, e5)
	}
}

func TestValidateUsageDateRange(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if err := ValidateUsageDateRange(from, to); err != nil {
		t.Fatalf("31-day range: %v", err)
	}
	toFar := from.AddDate(0, 0, 93)
	if err := ValidateUsageDateRange(from, toFar); err == nil {
		t.Fatal("expected error for 94-day inclusive span")
	}
	maxOK := from.AddDate(0, 0, 92) // 93 days inclusive
	if err := ValidateUsageDateRange(from, maxOK); err != nil {
		t.Fatalf("93-day range: %v", err)
	}
	if err := ValidateUsageDateRange(to, from); err == nil {
		t.Fatal("expected from>to error")
	}
}
