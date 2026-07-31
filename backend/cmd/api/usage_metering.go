package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/repository"
	"github.com/whatsappgetway/gateway/internal/service"
)

// recordUsageBestEffort incrementa usage_counters sem falhar a resposta ao cliente.
func (s *server) recordUsageBestEffort(
	ctx context.Context,
	systemID, tenantID string,
	dayUTC time.Time,
	d repository.UsageDelta,
	origin string,
) {
	if s.usageMeter == nil || d.IsZero() {
		return
	}
	systemID = strings.TrimSpace(systemID)
	tenantID = strings.TrimSpace(tenantID)
	if systemID == "" || tenantID == "" {
		return
	}
	if err := s.usageMeter.Increment(ctx, systemID, tenantID, dayUTC, d); err != nil {
		log.Printf(
			"usage counter lost (%s): system=%s tenant=%s: %v — request continues",
			origin, systemID, tenantID, err,
		)
	}
}

func usageDayUTC(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Truncate(24 * time.Hour)
}

func outboundSuccessDelta(templateName, msgType string) repository.UsageDelta {
	text, template, media := service.ClassifyOutbound(templateName, msgType)
	return repository.UsageDelta{
		SentText:     text,
		SentTemplate: template,
		SentMedia:    media,
	}
}

func outboundErrorDelta(err error) repository.UsageDelta {
	e4, e5 := service.ClassifySendError(err)
	return repository.UsageDelta{Errors4xx: e4, Errors5xx: e5}
}
