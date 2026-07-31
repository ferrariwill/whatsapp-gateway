package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
	"github.com/whatsappgetway/gateway/internal/provider"
)

const (
	defaultOutboundSweepInterval = 15 * time.Second
	defaultOutboundClaimTTL      = 2 * time.Minute
	defaultOutboundMaxAttempts   = 5
	defaultOutboundSweepBatch    = 50
)

func outboundBackoff(attempt int) time.Duration {
	// 1s, 5s, 30s, 2m, ...
	bases := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}
	if attempt <= 0 {
		attempt = 1
	}
	idx := attempt - 1
	if idx >= len(bases) {
		return bases[len(bases)-1] * time.Duration(math.Pow(2, float64(idx-len(bases)+1)))
	}
	return bases[idx]
}

func (s *server) maybeEnqueueOutboundRetry(
	ctx context.Context,
	conn *model.WhatsAppConnection,
	messageLog *model.MessageLog,
	kind model.OutboundRetryKind,
	payload map[string]any,
	sendErr error,
) {
	if !provider.IsMetaRetryable(sendErr) {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("outbound retry marshal: %v", err)
		return
	}
	item := &model.OutboundRetryQueueItem{
		MessageLogID:  messageLog.ID,
		SystemID:      conn.SystemID,
		ConnectionID:  conn.ID,
		Kind:          kind,
		PayloadJSON:   raw,
		Attempts:      0,
		NextAttemptAt: time.Now().UTC().Add(outboundBackoff(1)),
		LastError:     sendErr.Error(),
		Status:        model.OutboundRetryPending,
	}
	if err := s.repo.EnqueueOutboundRetry(ctx, item); err != nil {
		log.Printf("enqueue outbound retry for log %s: %v", messageLog.ID, err)
		return
	}
	code := "meta_retryable"
	if provider.IsMetaRateLimited(sendErr) {
		code = "meta_rate_limited"
	}
	_ = s.repo.UpdateMessageLogFailureCode(ctx, messageLog.ID, code, sendErr.Error())
	log.Printf("outbound retry queued log=%s kind=%s code=%s", messageLog.ID, kind, code)
}

func (s *server) runOutboundRetrySweeper(ctx context.Context) {
	interval := envDuration("OUTBOUND_RETRY_SWEEP_INTERVAL", defaultOutboundSweepInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("outbound Meta retry sweep: every %s", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.sweepOutboundRetries(ctx); err != nil {
				log.Printf("outbound retry sweep: %v", err)
			}
		}
	}
}

func (s *server) sweepOutboundRetries(ctx context.Context) (int, error) {
	items, err := s.repo.ClaimOutboundRetries(ctx, defaultOutboundSweepBatch, defaultOutboundClaimTTL)
	if err != nil {
		return 0, err
	}
	done := 0
	meta := provider.NewMetaProvider(s.metaClient, s.metaAPIVer)
	for i := range items {
		item := items[i]
		conn, err := s.repo.FindConnectionByID(ctx, item.ConnectionID)
		if err != nil {
			_ = s.repo.MarkOutboundRetryFailure(ctx, item.ID, item.Attempts+1, time.Now().UTC().Add(outboundBackoff(item.Attempts+1)), err.Error(), true)
			continue
		}
		metaID, sendErr := s.executeOutboundRetry(ctx, meta, conn, &item)
		attempts := item.Attempts + 1
		if sendErr == nil {
			if err := s.repo.MarkOutboundRetrySent(ctx, item.ID, metaID); err != nil {
				log.Printf("mark outbound retry sent %s: %v", item.ID, err)
			}
			done++
			continue
		}
		exhausted := attempts >= defaultOutboundMaxAttempts || !provider.IsMetaRetryable(sendErr)
		next := time.Now().UTC().Add(outboundBackoff(attempts + 1))
		if err := s.repo.MarkOutboundRetryFailure(ctx, item.ID, attempts, next, sendErr.Error(), exhausted); err != nil {
			log.Printf("mark outbound retry failure %s: %v", item.ID, err)
		}
	}
	return done, nil
}

func (s *server) executeOutboundRetry(
	ctx context.Context,
	meta *provider.MetaProvider,
	conn *model.WhatsAppConnection,
	item *model.OutboundRetryQueueItem,
) (string, error) {
	var payload map[string]any
	if err := json.Unmarshal(item.PayloadJSON, &payload); err != nil {
		return "", err
	}
	switch item.Kind {
	case model.OutboundRetryKindText:
		phone, _ := payload["phone_number"].(string)
		text, _ := payload["text"].(string)
		return meta.SendTextMessage(ctx, conn.AccessToken, conn.PhoneNumberID, phone, text)
	case model.OutboundRetryKindImage:
		phone, _ := payload["phone_number"].(string)
		link, _ := payload["link"].(string)
		mediaID, _ := payload["media_id"].(string)
		caption, _ := payload["caption"].(string)
		return meta.SendImageMessage(ctx, conn.AccessToken, conn.PhoneNumberID, phone, provider.ImageSendOpts{
			Link: link, MediaID: mediaID, Caption: caption,
		})
	case model.OutboundRetryKindDocument:
		phone, _ := payload["phone_number"].(string)
		link, _ := payload["link"].(string)
		mediaID, _ := payload["media_id"].(string)
		caption, _ := payload["caption"].(string)
		filename, _ := payload["filename"].(string)
		return meta.SendDocumentMessage(ctx, conn.AccessToken, conn.PhoneNumberID, phone, provider.DocumentSendOpts{
			Link: link, MediaID: mediaID, Caption: caption, Filename: filename,
		})
	case model.OutboundRetryKindPlainTemplate:
		phone, _ := payload["phone_number"].(string)
		name, _ := payload["template_name"].(string)
		lang, _ := payload["language_code"].(string)
		return meta.SendPlainTemplate(ctx, conn.AccessToken, conn.PhoneNumberID, phone, name, lang)
	default:
		phone, _ := payload["phone_number"].(string)
		name, _ := payload["template_name"].(string)
		vars := stringSlice(payload["variables"])
		return meta.SendAppointmentTemplate(ctx, conn.AccessToken, conn.PhoneNumberID, phone, name, vars)
	}
}

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}