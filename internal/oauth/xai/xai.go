// Package xai implements the public Grok-CLI OAuth device-code flow
// used by Pi's built-in xAI provider (RFC 8628 against auth.x.ai).
// Access tokens are Bearer credentials for https://api.x.ai/v1.
package xai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Public Grok-CLI OAuth client. This is a public client_id (not a secret);
// xAI only issues loopback/device tokens to allowlisted clients.
const (
	ClientID  = "b1a00492-073a-47ea-816f-4c329264a828"
	Scope     = "openid profile email offline_access grok-cli:access api:access"
	DeviceURL = "https://auth.x.ai/oauth2/device/code"
	TokenURL  = "https://auth.x.ai/oauth2/token"
	// Referrer identifies this client to xAI. Pi sends "pi"; prism must not
	// impersonate Pi.
	Referrer = "prism"

	RefreshSkew = 5 * time.Minute
	DefaultTTL  = time.Hour
)

// HTTPClient is the subset of http.Client used by the flow so tests can
// inject httptest clients.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Config holds endpoint overrides for tests. Zero values use production.
type Config struct {
	ClientID    string
	Scope       string
	DeviceURL   string
	TokenURL    string
	Referrer    string
	RefreshSkew time.Duration
	DefaultTTL  time.Duration
	HTTP        HTTPClient
}

func (c Config) withDefaults() Config {
	if c.ClientID == "" {
		c.ClientID = ClientID
	}
	if c.Scope == "" {
		c.Scope = Scope
	}
	if c.DeviceURL == "" {
		c.DeviceURL = DeviceURL
	}
	if c.TokenURL == "" {
		c.TokenURL = TokenURL
	}
	if c.Referrer == "" {
		c.Referrer = Referrer
	}
	if c.RefreshSkew == 0 {
		c.RefreshSkew = RefreshSkew
	}
	if c.DefaultTTL == 0 {
		c.DefaultTTL = DefaultTTL
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	return c
}

// Device holds the RFC 8628 device-authorization response.
type Device struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	Interval        time.Duration
	ExpiresIn       time.Duration
}

// Tokens is one access/refresh pair. ExpiresAt is already skewed.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
}

type deviceResponse struct {
	DeviceCode       string `json:"device_code"`
	UserCode         string `json:"user_code"`
	VerificationURI  string `json:"verification_uri"`
	Interval         int    `json:"interval"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

// RequestDevice starts the device-code flow.
func RequestDevice(ctx context.Context, cfg Config) (Device, error) {
	cfg = cfg.withDefaults()
	body := url.Values{
		"client_id": {cfg.ClientID},
		"scope":     {cfg.Scope},
		"referrer":  {cfg.Referrer},
	}
	var parsed deviceResponse
	if err := postForm(ctx, cfg.HTTP, cfg.DeviceURL, body, &parsed); err != nil {
		return Device{}, err
	}
	if parsed.Error != "" {
		return Device{}, fmt.Errorf("xAI OAuth device authorization failed: %s", errorDetail(parsed.Error, parsed.ErrorDescription))
	}
	uri, err := validateHTTPSURI(parsed.VerificationURI)
	if err != nil {
		return Device{}, err
	}
	if parsed.DeviceCode == "" || parsed.UserCode == "" {
		return Device{}, fmt.Errorf("xAI OAuth device authorization returned an incomplete payload")
	}
	if parsed.ExpiresIn <= 0 {
		return Device{}, fmt.Errorf("xAI OAuth device authorization returned an invalid expires_in")
	}
	interval := 5 * time.Second
	if parsed.Interval > 0 {
		interval = time.Duration(parsed.Interval) * time.Second
	}
	return Device{
		DeviceCode:      parsed.DeviceCode,
		UserCode:        parsed.UserCode,
		VerificationURI: uri,
		Interval:        interval,
		ExpiresIn:       time.Duration(parsed.ExpiresIn) * time.Second,
	}, nil
}

// PollToken waits until the user approves the device code and returns tokens.
func PollToken(ctx context.Context, cfg Config, device Device) (Tokens, error) {
	cfg = cfg.withDefaults()
	if device.Interval <= 0 {
		device.Interval = 5 * time.Second
	}
	deadline := time.Now().Add(device.ExpiresIn)
	if device.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	// RFC 8628: wait one interval before the first poll.
	if err := wait(ctx, device.Interval, deadline); err != nil {
		return Tokens{}, err
	}
	interval := device.Interval
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return Tokens{}, err
		}
		var parsed tokenResponse
		err := postForm(ctx, cfg.HTTP, cfg.TokenURL, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {cfg.ClientID},
			"device_code": {device.DeviceCode},
		}, &parsed)
		if err != nil {
			return Tokens{}, err
		}
		switch parsed.Error {
		case "":
			return tokensFromResponse(parsed, "", cfg, time.Now())
		case "authorization_pending":
			// keep polling
		case "slow_down":
			if parsed.Interval > 0 {
				interval = time.Duration(parsed.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
			if interval < time.Second {
				interval = time.Second
			}
		case "access_denied", "authorization_denied":
			return Tokens{}, fmt.Errorf("xAI device authorization was denied")
		case "expired_token":
			return Tokens{}, fmt.Errorf("xAI device code expired")
		default:
			return Tokens{}, fmt.Errorf("xAI OAuth device token polling failed: %s", errorDetail(parsed.Error, parsed.ErrorDescription))
		}
		if err := wait(ctx, interval, deadline); err != nil {
			return Tokens{}, err
		}
	}
	return Tokens{}, fmt.Errorf("xAI device flow timed out")
}

// Refresh exchanges a refresh_token for a new access token. When the
// response omits refresh_token, previousRefresh is kept (xAI may rotate).
func Refresh(ctx context.Context, cfg Config, refreshToken string) (Tokens, error) {
	cfg = cfg.withDefaults()
	if refreshToken == "" {
		return Tokens{}, fmt.Errorf("xAI OAuth refresh_token is empty")
	}
	var parsed tokenResponse
	err := postForm(ctx, cfg.HTTP, cfg.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cfg.ClientID},
		"refresh_token": {refreshToken},
	}, &parsed)
	if err != nil {
		return Tokens{}, err
	}
	if parsed.Error != "" {
		return Tokens{}, fmt.Errorf("xAI OAuth token refresh failed: %s", errorDetail(parsed.Error, parsed.ErrorDescription))
	}
	return tokensFromResponse(parsed, refreshToken, cfg, time.Now())
}

func tokensFromResponse(parsed tokenResponse, previousRefresh string, cfg Config, now time.Time) (Tokens, error) {
	if parsed.AccessToken == "" {
		return Tokens{}, fmt.Errorf("xAI OAuth token response missing access_token")
	}
	refresh := parsed.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	if refresh == "" {
		return Tokens{}, fmt.Errorf("xAI OAuth token response missing refresh_token")
	}
	ttl := cfg.DefaultTTL
	if parsed.ExpiresIn > 0 {
		ttl = time.Duration(parsed.ExpiresIn) * time.Second
	}
	exp := now.Add(ttl)
	if cfg.RefreshSkew > 0 && ttl > cfg.RefreshSkew {
		exp = now.Add(ttl - cfg.RefreshSkew)
	}
	return Tokens{Access: parsed.AccessToken, Refresh: refresh, ExpiresAt: exp}, nil
}

func postForm(ctx context.Context, client HTTPClient, endpoint string, fields url.Values, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(fields.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("xAI OAuth returned invalid JSON (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func validateHTTPSURI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("untrusted verification URI in xAI OAuth response")
	}
	return u.String(), nil
}

func errorDetail(code, desc string) string {
	code = strings.TrimSpace(code)
	desc = strings.TrimSpace(desc)
	switch {
	case code != "" && desc != "":
		return code + ": " + desc
	case desc != "":
		return desc
	default:
		return code
	}
}

func wait(ctx context.Context, d time.Duration, deadline time.Time) error {
	if d <= 0 {
		return nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("xAI device flow timed out")
	}
	if d > remaining {
		d = remaining
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
