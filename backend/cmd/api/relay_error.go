package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
)

const (
	failureReasonPermanent = "permanent"
	failureReasonTransient = "transient"
	failureReasonExhausted = "exhausted"
	failureReasonNoWebhook = "permanent" // ausência de destino é falha permanente de configuração
)

// relayError classifica falhas do POST ao SaaS para o DLQ.
type relayError struct {
	StatusCode int
	Body       string
	Err        error
	permanent  bool
}

func (e *relayError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		if e.StatusCode > 0 {
			return fmt.Sprintf("status %d: %v", e.StatusCode, e.Err)
		}
		return e.Err.Error()
	}
	if e.Body != "" {
		return fmt.Sprintf("status %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("status %d", e.StatusCode)
}

func (e *relayError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *relayError) Permanent() bool {
	return e != nil && e.permanent
}

func newRelayHTTPError(statusCode int, body string) *relayError {
	return &relayError{
		StatusCode: statusCode,
		Body:       strings.TrimSpace(body),
		permanent:  isPermanentHTTPStatus(statusCode),
	}
}

func newRelayTransportError(err error) *relayError {
	return &relayError{
		Err:       err,
		permanent: false, // timeout / rede / 429-classificados via HTTP
	}
}

func isPermanentHTTPStatus(code int) bool {
	switch code {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusGone,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func isPermanentRelayError(err error) bool {
	if err == nil {
		return false
	}
	var re *relayError
	if errors.As(err, &re) {
		return re.Permanent()
	}
	return false
}

func isTransientRelayError(err error) bool {
	if err == nil {
		return false
	}
	if isPermanentRelayError(err) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return true // default: treat unknown as transient for DLQ safety
}

func relayFailureReason(err error) string {
	if err == nil {
		return ""
	}
	if isPermanentRelayError(err) {
		return failureReasonPermanent
	}
	return failureReasonTransient
}

func readRelayErrorBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return strings.TrimSpace(string(body))
}
