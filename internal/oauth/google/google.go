// Package google refreshes Antigravity / Gemini OAuth tokens with the
// public Cloud Code client (the same client_id shipped in Antigravity).
// Access tokens are Bearer credentials for cloudcode-pa.googleapis.com.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Public Antigravity OAuth client. This is a public client_id (not a
// secret); Google only issues tokens to allowlisted clients. The client
// secret ships with the desktop client, but it is injected via the
// ANTIGRAVITY_OAUTH_CLIENT_SECRET environment variable rather than
// hard-coded here (secret scanners flag the Google OAuth secret format).
const (
	ClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	TokenURL = "https://oauth2.googleapis.com/token"

	RefreshSkew = 5 * time.Minute
	DefaultTTL  = time.Hour
)

// ClientSecretEnv is the env var carrying the public Antigravity desktop
// client secret. Absent → refresh requests fail with Google's
// "client_secret is missing".
const ClientSecretEnv = "ANTIGRAVITY_OAUTH_CLIENT_SECRET"

// HTTPClient is the subset of http.Client used by refresh so tests can
// inject httptest clients.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Config holds endpoint overrides for tests. Zero values use production.
type Config struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	RefreshSkew  time.Duration
	DefaultTTL   time.Duration
	HTTP         HTTPClient
}

func (c Config) withDefaults() Config {
	if c.ClientID == "" {
		c.ClientID = ClientID
	}
	if c.TokenURL == "" {
		c.TokenURL = TokenURL
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

// clientSecret resolves the desktop-client credential: an explicit Config
// value wins (tests, alternate clients), otherwise the environment variable.
func clientSecret(c Config) string {
	if c.ClientSecret != "" {
		return c.ClientSecret
	}
	return os.Getenv(ClientSecretEnv)
}

// Tokens is one access/refresh pair. ExpiresAt is already skewed.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Refresh exchanges a refresh_token for a new access token. When the
// response omits refresh_token, previousRefresh is kept.
func Refresh(ctx context.Context, cfg Config, refreshToken string) (Tokens, error) {
	cfg = cfg.withDefaults()
	if refreshToken == "" {
		return Tokens{}, fmt.Errorf("Google OAuth refresh_token is empty")
	}
	var parsed tokenResponse
	err := postForm(ctx, cfg.HTTP, cfg.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cfg.ClientID},
		"client_secret": {clientSecret(cfg)},
		"refresh_token": {refreshToken},
	}, &parsed)
	if err != nil {
		return Tokens{}, err
	}
	if parsed.Error != "" {
		return Tokens{}, fmt.Errorf("Google OAuth token refresh failed: %s", errorDetail(parsed.Error, parsed.ErrorDescription))
	}
	return tokensFromResponse(parsed, refreshToken, cfg, time.Now())
}

func tokensFromResponse(parsed tokenResponse, previousRefresh string, cfg Config, now time.Time) (Tokens, error) {
	if parsed.AccessToken == "" {
		return Tokens{}, fmt.Errorf("Google OAuth token response missing access_token")
	}
	refresh := parsed.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	if refresh == "" {
		return Tokens{}, fmt.Errorf("Google OAuth token response missing refresh_token")
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
		return fmt.Errorf("Google OAuth returned invalid JSON (HTTP %d)", resp.StatusCode)
	}
	return nil
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

// DefaultAgyTokenPath is the Antigravity CLI token file used by agy.
func DefaultAgyTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

type agyNested struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type agyFile struct {
	agyNested
	Token *agyNested `json:"token"`
}

// LoadAgyToken reads an Antigravity CLI token file
// (~/.gemini/antigravity-cli/antigravity-oauth-token).
func LoadAgyToken(path string) (Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tokens{}, err
	}
	var f agyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Tokens{}, fmt.Errorf("antigravity token file is not valid JSON")
	}
	nested := f.agyNested
	if f.Token != nil {
		nested = *f.Token
	}
	if nested.AccessToken == "" && nested.RefreshToken == "" {
		return Tokens{}, fmt.Errorf("antigravity token file missing access_token and refresh_token")
	}
	exp := nested.Expiry
	if exp.IsZero() {
		exp = nested.ExpiresAt
	}
	return Tokens{
		Access:    nested.AccessToken,
		Refresh:   nested.RefreshToken,
		ExpiresAt: exp,
	}, nil
}
