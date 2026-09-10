// Package google refreshes Antigravity / Gemini OAuth tokens with the
// public Cloud Code client (the same client_id shipped in Antigravity).
// Access tokens are Bearer credentials for cloudcode-pa.googleapis.com.
package google

import (
	"context"
	"encoding/json"
	"errors"
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

	// RefreshSkew is the lead time before access-token expiry at which
	// prism treats the token as stale. Google refresh tokens for this
	// client are durable (the token endpoint does not rotate them), so a
	// long lead time only increases refresh volume and can provoke
	// transient invalid_grant. 5 minutes matches the xAI source.
	RefreshSkew = 5 * time.Minute
	DefaultTTL  = time.Hour

	// CanonicalAgyTokenPath is the Antigravity CLI token file written by
	// agy running as root. The prism service runs as User=prism
	// (HOME=/home/prism), so a $HOME-relative lookup would miss it.
	// Production wiring (attachOAuth) sets this on at most one google
	// Source; NewSource itself does not, so tests never touch the live file.
	CanonicalAgyTokenPath = "/root/.gemini/antigravity-cli/antigravity-oauth-token"

	// CanonicalAgyTokenDir is the parent of CanonicalAgyTokenPath. systemd
	// ReadWritePaths must bind this directory (not the file) so tmp+rename
	// writes work under ProtectHome=true.
	CanonicalAgyTokenDir = "/root/.gemini/antigravity-cli"
)

// ErrAgyInvalidJSON is returned by StoreAgyToken when the file exists but
// is not valid JSON. The original bytes must be left untouched.
var ErrAgyInvalidJSON = errors.New("antigravity token file is not valid JSON; refusing to overwrite")

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
// Access-token freshness uses the package-level RefreshSkew constant, not a
// per-config field (skew is applied in SkewedExpiry, after Tokens are stored).
type Config struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
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
	if c.DefaultTTL == 0 {
		c.DefaultTTL = DefaultTTL
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	return c
}

// clientSecret resolves the desktop-client credential: an explicit Config
// value wins (tests, alternate clients), then the systemd LoadCredential
// directory (mirrors config.getCredential — on this host LoadCredential
// files are exposed under $CREDENTIALS_DIRECTORY, not as env vars), then
// the environment variable.
func clientSecret(c Config) string {
	if c.ClientSecret != "" {
		return c.ClientSecret
	}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		if data, err := os.ReadFile(filepath.Join(dir, ClientSecretEnv)); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return os.Getenv(ClientSecretEnv)
}

// Tokens is one access/refresh pair. ExpiresAt is the real wall-clock
// expiry (expires_in + now), not reduced by RefreshSkew. The agy token
// file must record that real expiry; prism applies RefreshSkew only when
// deciding whether the access token is still fresh.
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
	return Tokens{Access: parsed.AccessToken, Refresh: refresh, ExpiresAt: now.Add(ttl)}, nil
}

// SkewedExpiry is the instant at which prism should treat a google access
// token as stale. realExpiry is expires_in+now as stored on disk / in the
// agy file; freshness ends RefreshSkew before that instant.
func SkewedExpiry(realExpiry time.Time) time.Time {
	if RefreshSkew <= 0 {
		return realExpiry
	}
	return realExpiry.Add(-RefreshSkew)
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

// StoreAgyToken writes tok into an Antigravity CLI token file, keeping the
// nested {"token":{...}, "auth_method", "id_token", ...} envelope. Extra
// fields are preserved. A new refresh_token from the response wins; an
// empty tok.Refresh keeps the file's existing refresh_token. tok.ExpiresAt
// is written as-is (real expiry, not RefreshSkew).
//
// If the file exists but is not valid JSON, the original bytes are left
// untouched and ErrAgyInvalidJSON is returned.
//
// Write strategy: tmp+rename in the same directory when possible (systemd
// ReadWritePaths should bind the parent directory); if the directory is
// not writable, fall back to in-place truncate+write with fsync before
// Close. The caller must treat a returned error as non-fatal (log and
// keep the prism-side copy).
func StoreAgyToken(path string, tok Tokens) error {
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err == nil {
		if jerr := json.Unmarshal(data, &raw); jerr != nil {
			return ErrAgyInvalidJSON
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	tokenObj := map[string]json.RawMessage{}
	if nested, ok := raw["token"]; ok && len(nested) > 0 && nested[0] == '{' {
		_ = json.Unmarshal(nested, &tokenObj)
	} else {
		for _, k := range []string{"access_token", "refresh_token", "token_type", "expiry", "expires_at"} {
			if v, ok := raw[k]; ok {
				tokenObj[k] = v
				delete(raw, k)
			}
		}
	}

	if tok.Access != "" {
		tokenObj["access_token"] = mustJSON(tok.Access)
	}
	rt := tok.Refresh
	if rt == "" {
		if old, ok := tokenObj["refresh_token"]; ok {
			_ = json.Unmarshal(old, &rt)
		}
	}
	if rt != "" {
		tokenObj["refresh_token"] = mustJSON(rt)
	}
	if !tok.ExpiresAt.IsZero() {
		exp := mustJSON(tok.ExpiresAt)
		tokenObj["expiry"] = exp
		if _, ok := tokenObj["expires_at"]; ok {
			tokenObj["expires_at"] = exp
		}
	}
	if _, ok := tokenObj["token_type"]; !ok {
		tokenObj["token_type"] = mustJSON("Bearer")
	}

	nestedBytes, err := json.Marshal(tokenObj)
	if err != nil {
		return err
	}
	raw["token"] = nestedBytes
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return writeAgyFile(path, out)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func writeAgyFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agy-oauth-*.tmp")
	if err != nil {
		return writeAgyInPlace(path, data)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return writeAgyInPlace(path, data)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return writeAgyInPlace(path, data)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return writeAgyInPlace(path, data)
	}
	if err := tmp.Close(); err != nil {
		return writeAgyInPlace(path, data)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return writeAgyInPlace(path, data)
	}
	ok = true
	return nil
}

func writeAgyInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
