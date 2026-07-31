package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newFakeMetaClient(t *testing.T, handler roundTripFunc) *http.Client {
	t.Helper()
	return &http.Client{Transport: handler, Timeout: 5 * time.Second}
}

func metaErrorBody(code int, message string) string {
	payload := map[string]any{
		"error": map[string]any{
			"message":    message,
			"type":       "OAuthException",
			"code":       code,
			"fbtrace_id": "AbC123",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

const qaAccessToken = "EAAG-super-secret-tenant-token-do-not-leak"

// TestSendAppointmentTemplateMetaHTTPFailures cobre as quedas de rede/Meta que
// o gateway precisa tratar sem pânico e sem devolver wamid falso.
func TestSendAppointmentTemplateMetaHTTPFailures(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantContains []string
	}{
		{
			name:         "500 internal error",
			status:       http.StatusInternalServerError,
			body:         metaErrorBody(1, "An unknown error occurred"),
			wantContains: []string{"status 500", "An unknown error occurred"},
		},
		{
			name:         "429 rate limited by meta",
			status:       http.StatusTooManyRequests,
			body:         metaErrorBody(130429, "Rate limit hit"),
			wantContains: []string{"status 429", "Rate limit hit", "130429"},
		},
		{
			name:         "503 empty body",
			status:       http.StatusServiceUnavailable,
			body:         "",
			wantContains: []string{"status 503"},
		},
		{
			name:         "502 html gateway page",
			status:       http.StatusBadGateway,
			body:         "<html><body>502 Bad Gateway</body></html>",
			wantContains: []string{"status 502"},
		},
		{
			name:         "401 invalid token",
			status:       http.StatusUnauthorized,
			body:         metaErrorBody(190, "Invalid OAuth access token"),
			wantContains: []string{"status 401", "Invalid OAuth access token"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeMetaClient(t, func(_ *http.Request) (*http.Response, error) {
				return jsonResponse(tc.status, tc.body), nil
			})

			messageID, err := NewMetaProvider(client, "v21.0").SendAppointmentTemplate(
				context.Background(), qaAccessToken, "phone-1", "5511999999999", "confirm_appointment", []string{"Ana"},
			)
			if err == nil {
				t.Fatalf("expected error for status %d, got message id %q", tc.status, messageID)
			}
			if messageID != "" {
				t.Errorf("message id must be empty on failure, got %q", messageID)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should contain %q", err.Error(), want)
				}
			}
			if strings.Contains(err.Error(), qaAccessToken) {
				t.Errorf("access token leaked into error message: %v", err)
			}
		})
	}
}

// TestSendAppointmentTemplateTransportFailures cobre queda de conexão, timeout e
// cancelamento de contexto (túnel/rede indisponível).
func TestSendAppointmentTemplateTransportFailures(t *testing.T) {
	t.Run("connection reset", func(t *testing.T) {
		client := newFakeMetaClient(t, func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset by peer")
		})

		_, err := NewMetaProvider(client, "v21.0").SendAppointmentTemplate(
			context.Background(), qaAccessToken, "phone-1", "5511999999999", "confirm_appointment", nil,
		)
		if err == nil {
			t.Fatal("expected transport error")
		}
		if !strings.Contains(err.Error(), "execute request") {
			t.Errorf("transport error should be wrapped, got %v", err)
		}
		if strings.Contains(err.Error(), qaAccessToken) {
			t.Errorf("access token leaked into error message: %v", err)
		}
	})

	t.Run("context deadline exceeded", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		client := newFakeMetaClient(t, func(r *http.Request) (*http.Response, error) {
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-release:
				return jsonResponse(http.StatusOK, `{"messages":[{"id":"wamid.late"}]}`), nil
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		messageID, err := NewMetaProvider(client, "v21.0").SendAppointmentTemplate(
			ctx, qaAccessToken, "phone-1", "5511999999999", "confirm_appointment", nil,
		)
		if err == nil {
			t.Fatalf("expected timeout error, got message id %q", messageID)
		}
		if messageID != "" {
			t.Errorf("message id must be empty on timeout, got %q", messageID)
		}
	})

	t.Run("context already cancelled", func(t *testing.T) {
		client := newFakeMetaClient(t, func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			return jsonResponse(http.StatusOK, `{"messages":[{"id":"wamid.OK"}]}`), nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := NewMetaProvider(client, "v21.0").SendAppointmentTemplate(
			ctx, qaAccessToken, "phone-1", "5511999999999", "confirm_appointment", nil,
		); err == nil {
			t.Fatal("expected error for cancelled context")
		}
	})
}

// TestSendAppointmentTemplateMalformedSuccessBodies garante que resposta 200
// inconsistente da Meta não vira "enviado com sucesso" sem wamid.
func TestSendAppointmentTemplateMalformedSuccessBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty messages array", body: `{"messages":[]}`},
		{name: "blank message id", body: `{"messages":[{"id":"   "}]}`},
		{name: "missing messages field", body: `{"messaging_product":"whatsapp"}`},
		{name: "not json", body: `not-json-at-all`},
		{name: "empty body", body: ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeMetaClient(t, func(_ *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, tc.body), nil
			})

			messageID, err := NewMetaProvider(client, "v21.0").SendAppointmentTemplate(
				context.Background(), qaAccessToken, "phone-1", "5511999999999", "confirm_appointment", nil,
			)
			if err == nil {
				t.Fatalf("expected error for body %q, got message id %q", tc.body, messageID)
			}
			if messageID != "" {
				t.Errorf("message id must be empty, got %q", messageID)
			}
		})
	}
}

// TestTemplateAPIFailuresArePropagated cobre 429/500 nas rotas de templates.
func TestTemplateAPIFailuresArePropagated(t *testing.T) {
	client := newFakeMetaClient(t, func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, metaErrorBody(4, "Application request limit reached")), nil
	})
	meta := NewMetaProvider(client, "v21.0")

	if _, err := meta.CreateTemplate(
		context.Background(), qaAccessToken, "waba-1", "confirm_appointment", "UTILITY", "Olá {{1}}", []string{"Confirmar"},
	); err == nil || !strings.Contains(err.Error(), "status 429") {
		t.Errorf("CreateTemplate should surface the 429, got %v", err)
	}

	if _, err := meta.GetTemplatesStatus(context.Background(), qaAccessToken, "waba-1"); err == nil ||
		!strings.Contains(err.Error(), "status 429") {
		t.Errorf("GetTemplatesStatus should surface the 429, got %v", err)
	}

	if _, err := meta.SendTextMessage(
		context.Background(), qaAccessToken, "phone-1", "5511999999999", "alerta",
	); err == nil || !strings.Contains(err.Error(), "status 429") {
		t.Errorf("SendTextMessage should surface the 429, got %v", err)
	}
}

// TestMetaRequestCarriesTenantCredentials valida que cada chamada usa o token e
// o phone_number_id da conexão informada, e a versão configurada da API.
func TestMetaRequestCarriesTenantCredentials(t *testing.T) {
	var gotAuth, gotURL, gotContentType string
	var gotBody []byte

	client := newFakeMetaClient(t, func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotURL = r.URL.String()
		gotBody, _ = io.ReadAll(r.Body)
		return jsonResponse(http.StatusOK, `{"messages":[{"id":"wamid.OK"}]}`), nil
	})

	messageID, err := NewMetaProvider(client, "v21.0").SendAppointmentTemplate(
		context.Background(), qaAccessToken, "phone-number-id-42", "5511999999999", "confirm_appointment", []string{"Ana", "10:00"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if messageID != "wamid.OK" {
		t.Errorf("message id = %q, want wamid.OK", messageID)
	}
	if gotAuth != "Bearer "+qaAccessToken {
		t.Errorf("Authorization = %q, want tenant bearer token", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if want := "https://graph.facebook.com/v21.0/phone-number-id-42/messages"; gotURL != want {
		t.Errorf("url = %q, want %q", gotURL, want)
	}

	var sent SendMessageRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.MessagingProduct != "whatsapp" || sent.Type != "template" {
		t.Errorf("unexpected envelope: %+v", sent)
	}
	if sent.Template.Language.Code != defaultLanguageCode {
		t.Errorf("language = %q, want %q", sent.Template.Language.Code, defaultLanguageCode)
	}
	if len(sent.Template.Components) != 3 {
		t.Fatalf("expected body + 2 quick reply buttons, got %d components", len(sent.Template.Components))
	}
	if len(sent.Template.Components[0].Parameters) != 2 {
		t.Errorf("body should carry the 2 variables, got %+v", sent.Template.Components[0].Parameters)
	}
}

// TestMetaProviderTokenIsolationUnderConcurrency garante que o provider
// stateless não mistura credenciais de tenants em disparos simultâneos.
func TestMetaProviderTokenIsolationUnderConcurrency(t *testing.T) {
	const tenants = 32
	const callsPerTenant = 8

	var mu sync.Mutex
	// pares observados na requisição: token → phone_number_id
	observed := map[string]map[string]int{}

	client := newFakeMetaClient(t, func(r *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		phoneNumberID := parts[len(parts)-2]

		mu.Lock()
		if observed[token] == nil {
			observed[token] = map[string]int{}
		}
		observed[token][phoneNumberID]++
		mu.Unlock()

		return jsonResponse(http.StatusOK, `{"messages":[{"id":"wamid.OK"}]}`), nil
	})

	meta := NewMetaProvider(client, "v21.0")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range tenants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("token-tenant-%02d", i)
			phoneNumberID := fmt.Sprintf("phone-%02d", i)
			<-start
			for range callsPerTenant {
				if _, err := meta.SendAppointmentTemplate(
					context.Background(), token, phoneNumberID, "5511999999999", "confirm_appointment", []string{"Ana"},
				); err != nil {
					t.Errorf("tenant %d send failed: %v", i, err)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(observed) != tenants {
		t.Fatalf("expected %d distinct tokens on the wire, got %d", tenants, len(observed))
	}
	for i := range tenants {
		token := fmt.Sprintf("token-tenant-%02d", i)
		wantPhone := fmt.Sprintf("phone-%02d", i)
		pairs, ok := observed[token]
		if !ok {
			t.Errorf("token %s never reached Meta", token)
			continue
		}
		if len(pairs) != 1 {
			t.Errorf("credential leak: token %s was used with phone_number_ids %v", token, pairs)
			continue
		}
		if got := pairs[wantPhone]; got != callsPerTenant {
			t.Errorf("token %s + %s: got %d calls, want %d (pairs=%v)", token, wantPhone, got, callsPerTenant, pairs)
		}
	}
}

// TestMetaProviderRejectsIncompleteConnection garante validação local antes de
// gastar chamada de rede quando a conexão do tenant está incompleta no banco.
func TestMetaProviderRejectsIncompleteConnection(t *testing.T) {
	client := newFakeMetaClient(t, func(_ *http.Request) (*http.Response, error) {
		t.Error("no request should reach Meta with incomplete connection data")
		return jsonResponse(http.StatusOK, `{}`), nil
	})
	meta := NewMetaProvider(client, "v21.0")

	cases := []struct {
		name                           string
		token, phoneNumberID, to, tmpl string
	}{
		{name: "missing token", phoneNumberID: "p", to: "5511", tmpl: "t"},
		{name: "missing phone number id", token: "tok", to: "5511", tmpl: "t"},
		{name: "missing recipient", token: "tok", phoneNumberID: "p", tmpl: "t"},
		{name: "missing template", token: "tok", phoneNumberID: "p", to: "5511"},
		{name: "whitespace only token", token: "   ", phoneNumberID: "p", to: "5511", tmpl: "t"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := meta.SendAppointmentTemplate(
				context.Background(), tc.token, tc.phoneNumberID, tc.to, tc.tmpl, nil,
			); err == nil {
				t.Fatal("expected local validation error")
			}
		})
	}
}
