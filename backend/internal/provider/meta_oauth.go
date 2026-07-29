package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type OAuthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type debugTokenResponse struct {
	Data struct {
		GranularScopes []struct {
			Scope     string   `json:"scope"`
			TargetIDs []string `json:"target_ids"`
		} `json:"granular_scopes"`
	} `json:"data"`
}

type wabaPhoneNumbersResponse struct {
	Data []struct {
		ID                 string `json:"id"`
		DisplayPhoneNumber string `json:"display_phone_number"`
		VerifiedName       string `json:"verified_name"`
	} `json:"data"`
}

// EmbeddedSignupAssets credenciais resolvidas após Embedded Signup da Meta.
type EmbeddedSignupAssets struct {
	WabaID              string
	PhoneNumberID       string
	WhatsAppPhoneNumber string
	AccessToken         string
}

// ExchangeOAuthCode troca o code do Embedded Signup por access token de longa duração.
func (p *MetaProvider) ExchangeOAuthCode(
	ctx context.Context,
	appID, appSecret, code, redirectURI string,
) (string, error) {
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	code = strings.TrimSpace(code)
	redirectURI = strings.TrimSpace(redirectURI)
	if appID == "" || appSecret == "" {
		return "", fmt.Errorf("meta app id and secret are required")
	}
	if code == "" {
		return "", fmt.Errorf("oauth code is required")
	}
	if redirectURI == "" {
		return "", fmt.Errorf("redirect uri is required")
	}

	endpoint := fmt.Sprintf("%s/%s/oauth/access_token", graphAPIBaseURL, p.apiVersion)
	form := url.Values{}
	form.Set("client_id", appID)
	form.Set("client_secret", appSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute oauth request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read oauth response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", parseMetaHTTPError(resp.StatusCode, body)
	}

	var token OAuthTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("decode oauth response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return "", fmt.Errorf("meta oauth returned empty access token")
	}
	return strings.TrimSpace(token.AccessToken), nil
}

// ResolveEmbeddedSignupAssets obtém WABA e phone_number_id a partir do token do cliente.
func (p *MetaProvider) ResolveEmbeddedSignupAssets(
	ctx context.Context,
	appID, appSecret, accessToken string,
) (EmbeddedSignupAssets, error) {
	wabaID, err := p.resolveWABAIDFromToken(ctx, appID, appSecret, accessToken)
	if err != nil {
		return EmbeddedSignupAssets{}, err
	}

	phoneNumberID, displayPhone, err := p.resolvePrimaryPhoneNumber(ctx, accessToken, wabaID)
	if err != nil {
		return EmbeddedSignupAssets{}, err
	}

	if err := p.SubscribeAppToWABA(ctx, accessToken, wabaID); err != nil {
		return EmbeddedSignupAssets{}, err
	}

	return EmbeddedSignupAssets{
		WabaID:              wabaID,
		PhoneNumberID:       phoneNumberID,
		WhatsAppPhoneNumber: displayPhone,
		AccessToken:         accessToken,
	}, nil
}

func (p *MetaProvider) resolveWABAIDFromToken(ctx context.Context, appID, appSecret, accessToken string) (string, error) {
	appToken := appID + "|" + appSecret
	endpoint := fmt.Sprintf(
		"%s/%s/debug_token?input_token=%s",
		graphAPIBaseURL,
		p.apiVersion,
		url.QueryEscape(accessToken),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("create debug token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute debug token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read debug token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", parseMetaHTTPError(resp.StatusCode, body)
	}

	var debugResp debugTokenResponse
	if err := json.Unmarshal(body, &debugResp); err != nil {
		return "", fmt.Errorf("decode debug token response: %w", err)
	}

	for _, scope := range debugResp.Data.GranularScopes {
		if strings.Contains(scope.Scope, "whatsapp") && len(scope.TargetIDs) > 0 {
			return strings.TrimSpace(scope.TargetIDs[0]), nil
		}
	}
	return "", fmt.Errorf("waba id not found in token granular scopes")
}

func (p *MetaProvider) resolvePrimaryPhoneNumber(ctx context.Context, accessToken, wabaID string) (phoneNumberID, displayPhone string, err error) {
	endpoint := fmt.Sprintf(
		"%s/%s/%s/phone_numbers?fields=id,display_phone_number,verified_name",
		graphAPIBaseURL,
		p.apiVersion,
		wabaID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("create phone numbers request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("execute phone numbers request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read phone numbers response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", parseMetaHTTPError(resp.StatusCode, body)
	}

	var listed wabaPhoneNumbersResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		return "", "", fmt.Errorf("decode phone numbers response: %w", err)
	}
	if len(listed.Data) == 0 {
		return "", "", fmt.Errorf("no phone numbers found for waba %s", wabaID)
	}

	phone := listed.Data[0]
	return strings.TrimSpace(phone.ID), strings.TrimSpace(phone.DisplayPhoneNumber), nil
}

// SubscribeAppToWABA inscreve o app nos webhooks da WABA (exigido pelo Embedded Signup).
func (p *MetaProvider) SubscribeAppToWABA(ctx context.Context, accessToken, wabaID string) error {
	endpoint := fmt.Sprintf("%s/%s/%s/subscribed_apps", graphAPIBaseURL, p.apiVersion, wabaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create subscribed_apps request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("execute subscribed_apps request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read subscribed_apps response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return parseMetaHTTPError(resp.StatusCode, body)
	}
	return nil
}
