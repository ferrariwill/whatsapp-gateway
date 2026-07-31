package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// htmx 2.x does not swap [45]xx by default; volume error HTML must be 200 so
// #usage-volume-card receives the retry partial (DEV-153 QA repro).
func TestWriteUsageVolumeErrorReturnsOKForHTMXSwap(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		rec := httptest.NewRecorder()
		writeUsageVolumeError(rec, status, "Falha ao carregar volume de disparos.")

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: got HTTP %d, want 200 so htmx swaps into #usage-volume-card", status, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("status %d: Content-Type = %q, want text/html", status, ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Falha ao carregar volume de disparos.") {
			t.Fatalf("status %d: body missing error message: %s", status, body)
		}
		if !strings.Contains(body, "Tentar novamente") {
			t.Fatalf("status %d: body missing retry button: %s", status, body)
		}
		if !strings.Contains(body, `hx-target="#usage-volume-card"`) {
			t.Fatalf("status %d: retry button missing hx-target: %s", status, body)
		}
	}
}
