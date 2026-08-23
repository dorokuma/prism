// Package oauth stores live OAuth tokens on disk and refreshes them for
// the request path. Tokens are never written to YAML or logs.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/oauth/xai"
)

// DefaultDir is the on-disk token directory (0600 files, 0700 dir).
const DefaultDir = "/var/lib/prism/oauth"

// ErrNotLoggedIn is returned when the account has oauth: xai but no
// token file yet (`prism auth xai` has not been run).
var ErrNotLoggedIn = errors.New("oauth: not logged in")

// File is the JSON document written under DefaultDir.
type File struct {
	Provider     string    `json:"provider"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// RefreshFunc exchanges a refresh token for a new pair.
type RefreshFunc func(ctx context.Context, refreshToken string) (xai.Tokens, error)

// Source is a pool.TokenSource backed by a per-account JSON file.
type Source struct {
	mu       sync.Mutex
	dir      string
	account  string
	provider string
	refresh  RefreshFunc
	now      func() time.Time

	cred   File
	mtime  time.Time
	loaded bool
}

// NewXAISource builds a file-backed source for one xai oauth account.
func NewXAISource(dir, account string, refresh RefreshFunc) *Source {
	if dir == "" {
		dir = DefaultDir
	}
	return &Source{
		dir:      dir,
		account:  account,
		provider: "xai",
		refresh:  refresh,
		now:      time.Now,
	}
}

func (s *Source) path() string {
	return filepath.Join(s.dir, s.account+".json")
}

// Token returns a non-expired access token, refreshing and rewriting the
// file when needed. A login completed by `prism auth xai` is picked up via
// the file mtime without a process restart.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return "", err
	}
	if s.cred.AccessToken == "" || s.cred.RefreshToken == "" {
		return "", fmt.Errorf("%w (run: prism auth xai --account %s)", ErrNotLoggedIn, s.account)
	}
	if s.now().Before(s.cred.ExpiresAt) {
		return s.cred.AccessToken, nil
	}
	if s.refresh == nil {
		return "", fmt.Errorf("oauth: token expired and no refresher is configured")
	}
	tok, err := s.refresh(ctx, s.cred.RefreshToken)
	if err != nil {
		return "", err
	}
	next := File{
		Provider:     s.provider,
		AccessToken:  tok.Access,
		RefreshToken: tok.Refresh,
		ExpiresAt:    tok.ExpiresAt,
	}
	if err := writeFile(s.path(), next); err != nil {
		return "", err
	}
	s.cred = next
	if fi, err := os.Stat(s.path()); err == nil {
		s.mtime = fi.ModTime()
	}
	s.loaded = true
	return s.cred.AccessToken, nil
}

func (s *Source) reloadLocked() error {
	path := s.path()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.cred = File{}
			s.loaded = true
			s.mtime = time.Time{}
			return nil
		}
		return err
	}
	if s.loaded && fi.ModTime().Equal(s.mtime) {
		return nil
	}
	cred, err := readFile(path)
	if err != nil {
		return err
	}
	s.cred = cred
	s.mtime = fi.ModTime()
	s.loaded = true
	return nil
}

// Save writes tokens for account under dir. Used by `prism auth xai`.
func Save(dir, account, provider string, tok xai.Tokens) error {
	if err := config.ValidateAccountName(account); err != nil {
		return fmt.Errorf("oauth account: %w", err)
	}
	if dir == "" {
		dir = DefaultDir
	}
	if provider == "" {
		provider = "xai"
	}
	return writeFile(filepath.Join(dir, account+".json"), File{
		Provider:     provider,
		AccessToken:  tok.Access,
		RefreshToken: tok.Refresh,
		ExpiresAt:    tok.ExpiresAt,
	})
}

func readFile(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("oauth token file is not valid JSON")
	}
	return f, nil
}

func writeFile(path string, f File) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// `prism auth` is typically run as root; the service is User=prism.
	// Align the directory with its parent so a newly created oauth dir
	// is not left root:root 0700 (the service could not even list it).
	if parent := filepath.Dir(dir); parent != "" && parent != dir {
		if err := chownLike(dir, parent); err != nil && !os.IsPermission(err) {
			return fmt.Errorf("chown oauth dir to parent owner: %w", err)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".oauth-*.tmp")
	if err != nil {
		return err
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
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := chownLike(path, dir); err != nil {
		_ = os.Remove(path)
		ok = true
		return fmt.Errorf("chown oauth token to directory owner: %w", err)
	}
	ok = true
	return nil
}

// chownLike sets path's uid/gid to match like. No-op when already matching
// or when Sys() is not a syscall.Stat_t.
func chownLike(path, like string) error {
	fi, err := os.Stat(like)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	wantUID, wantGID := int(st.Uid), int(st.Gid)
	cur, err := os.Stat(path)
	if err != nil {
		return err
	}
	if cst, ok := cur.Sys().(*syscall.Stat_t); ok && int(cst.Uid) == wantUID && int(cst.Gid) == wantGID {
		return nil
	}
	return os.Chown(path, wantUID, wantGID)
}
