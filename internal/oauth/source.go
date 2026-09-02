// Package oauth stores live OAuth tokens on disk and refreshes them for
// the request path. Tokens are never written to YAML or logs.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/oauth/xai"
)

// DefaultDir is the on-disk token directory (0600 files, 0700 dir).
const DefaultDir = "/var/lib/prism/oauth"

// ErrNotLoggedIn is returned when the account has oauth: xai/google but no
// token file yet (`prism auth xai` / `prism auth google` has not been run).
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

	terminalInvalid bool
}

// NewSource builds a file-backed source for one oauth account.
func NewSource(dir, account, provider string, refresh RefreshFunc) *Source {
	if dir == "" {
		dir = DefaultDir
	}
	if provider == "" {
		provider = "xai"
	}
	return &Source{
		dir:      dir,
		account:  account,
		provider: provider,
		refresh:  refresh,
		now:      time.Now,
	}
}

// NewXAISource builds a file-backed source for one xai oauth account.
func NewXAISource(dir, account string, refresh RefreshFunc) *Source {
	return NewSource(dir, account, "xai", refresh)
}

func (s *Source) loginHint() string {
	p := strings.TrimSpace(s.provider)
	if p == "" {
		p = "xai"
	}
	return fmt.Sprintf("prism auth %s --account %s", p, s.account)
}

func (s *Source) path() string {
	return filepath.Join(s.dir, s.account+".json")
}

func (s *Source) lockPath() string    { return s.path() + ".lock" }
func (s *Source) invalidPath() string { return s.path() + ".invalid" }

// bootstrapOAuthDir creates the oauth directory when missing and aligns its
// owner with its parent (mirrors the directory handling in writeFile, moved
// here so the LOCK file created below already belongs to the directory's
// final owner — the login CLI usually runs as root while the service runs
// as the prism user).
func bootstrapOAuthDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if parent := filepath.Dir(dir); parent != "" && parent != dir {
		if err := chownLike(dir, parent); err != nil && !os.IsPermission(err) {
			return fmt.Errorf("chown oauth dir to parent owner: %w", err)
		}
	}
	return nil
}

// lockFile takes an exclusive flock on path, creating the file when
// missing. O_CREATE is load-bearing: deployments that predate the lock
// file have a token file but no .lock, so the first refresh must
// bootstrap the lock instead of failing. The lock's owner is aligned with
// the directory (same treatment as the token file).
func lockFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := bootstrapOAuthDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := chownLike(path, dir); err != nil && !os.IsPermission(err) {
		_ = f.Close()
		return nil, fmt.Errorf("chown oauth lock to directory owner: %w", err)
	}
	return f, nil
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func (s *Source) withFileLock(fn func() error) error {
	f, err := lockFile(s.lockPath())
	if err != nil {
		return err
	}
	defer unlockFile(f)
	return fn()
}

func (s *Source) reloadFromDiskLocked() error {
	cred, err := readFile(s.path())
	if err != nil {
		return err
	}
	// The in-memory pair may be NEWER than the disk one: a refresh
	// consumed the refresh token and then the persist failed, leaving
	// the disk with the dead-on-arrival pair. Re-reading it unconditionally
	// (the old behavior) would resurrect the consumed refresh token — the
	// next refresh would be a guaranteed invalid_grant and would trip the
	// terminal latch even though a valid pair is live in memory. Adopt the
	// disk state only when it is at least as fresh: for constant-TTL
	// providers like xAI a later rotation always extends ExpiresAt, so the
	// comparison is unambiguous (memory already empty → always adopt).
	if s.cred.RefreshToken == "" || cred.ExpiresAt.After(s.cred.ExpiresAt) {
		s.cred = cred
	}
	if fi, e := os.Stat(s.path()); e == nil {
		s.mtime = fi.ModTime()
	}
	s.loaded = true
	s.syncTerminalInvalidFromDisk()
	return nil
}

// syncTerminalInvalidFromDisk keeps the in-memory terminal latch in
// lockstep with the on-disk .invalid marker in BOTH directions: a refresh
// failure sets the latch and writes the marker; a re-login (Save removes
// the marker and writes a fresh token) must clear the latch WITHOUT a
// process restart. A one-way latch (only ever set) strands the account in
// the terminal state after login until the service is restarted.
func (s *Source) syncTerminalInvalidFromDisk() {
	_, err := os.Stat(s.invalidPath())
	s.terminalInvalid = (err == nil)
}

func isTerminalRefreshError(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "invalid_grant") || strings.Contains(text, "invalid or unknown refresh token")
}

func (s *Source) refreshLocked(ctx context.Context, force bool) (string, error) {
	if s.terminalInvalid {
		return "", fmt.Errorf("oauth: terminal token invalid (run: %s)", s.loginHint())
	}
	if !force && s.now().Before(s.cred.ExpiresAt) {
		return s.cred.AccessToken, nil
	}
	tok, err := s.refresh(ctx, s.cred.RefreshToken)
	if err != nil {
		if isTerminalRefreshError(err) {
			s.terminalInvalid = true
			_ = os.WriteFile(s.invalidPath(), []byte("invalid\n"), 0o600)
		}
		return "", err
	}
	next := File{Provider: s.provider, AccessToken: tok.Access, RefreshToken: tok.Refresh, ExpiresAt: tok.ExpiresAt}
	// The refresh token was just CONSUMED by the rotation: when the
	// persist fails, adopt the new pair in memory anyway and keep working
	// (the next successful refresh re-persists it). Returning the error
	// here would strand the process on the OLD refresh token, which is
	// dead on arrival — the next refresh would be a guaranteed
	// invalid_grant and would trip the terminal latch even though the
	// account is perfectly recoverable from memory.
	if err := writeFileFn(s.path(), next); err != nil {
		slog.Warn("oauth token persist failed, keeping new token in memory", "account", s.account, "error", err)
	}
	s.cred = next
	s.loaded = true
	s.terminalInvalid = false
	if fi, e := os.Stat(s.path()); e == nil {
		s.mtime = fi.ModTime()
	}
	_ = os.Remove(s.invalidPath())
	return next.AccessToken, nil
}

// writeFileFn is the token-file write used by refreshLocked. It is a
// variable (not a direct call) so tests can simulate a failed persist
// (full disk, EROFS, ...) and assert the in-memory token survives;
// production always uses writeFile.
var writeFileFn = writeFile

// ForceRefresh rotates the token regardless of its expiry time.
func (s *Source) ForceRefresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result string
	err := s.withFileLock(func() error {
		if err := s.reloadFromDiskLocked(); err != nil {
			return err
		}
		var err error
		result, err = s.refreshLocked(ctx, true)
		return err
	})
	return result, err
}

// OAuthTerminalInvalid reports a terminal refresh failure until login.
func (s *Source) OAuthTerminalInvalid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalInvalid
}

// RefreshIfStale is the 401-reactive refresh. It rotates only when the
// on-disk access token is still the one the upstream just rejected
// (staleToken). When a concurrent 401 handler (or the periodic keepalive)
// already rotated, the disk holds a different token and it is reused
// as-is — the single-use refresh token is never rotated twice for the
// same rejection (N concurrent 401s must burn exactly one rotation).
func (s *Source) RefreshIfStale(ctx context.Context, staleToken string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result string
	err := s.withFileLock(func() error {
		if err := s.reloadFromDiskLocked(); err != nil {
			return err
		}
		if s.terminalInvalid {
			return fmt.Errorf("oauth: terminal token invalid (run: %s)", s.loginHint())
		}
		if s.cred.AccessToken == "" || s.cred.RefreshToken == "" {
			return fmt.Errorf("%w (run: %s)", ErrNotLoggedIn, s.loginHint())
		}
		if s.cred.AccessToken != staleToken {
			// A concurrent caller already rotated: reuse its result.
			result = s.cred.AccessToken
			return nil
		}
		var refreshErr error
		result, refreshErr = s.refreshLocked(ctx, true)
		return refreshErr
	})
	return result, err
}

// Token returns a non-expired access token, refreshing and rewriting the
// file when needed. A login completed by `prism auth` is picked up via
// the file mtime without a process restart.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return "", err
	}
	if s.terminalInvalid {
		return "", fmt.Errorf("oauth: terminal token invalid (run: %s)", s.loginHint())
	}
	if s.cred.AccessToken == "" || s.cred.RefreshToken == "" {
		return "", fmt.Errorf("%w (run: %s)", ErrNotLoggedIn, s.loginHint())
	}
	if s.now().Before(s.cred.ExpiresAt) {
		return s.cred.AccessToken, nil
	}
	if s.refresh == nil {
		return "", fmt.Errorf("oauth: token expired and no refresher is configured")
	}
	var result string
	err := s.withFileLock(func() error {
		if err := s.reloadFromDiskLocked(); err != nil {
			return err
		}
		if s.cred.AccessToken == "" || s.cred.RefreshToken == "" {
			return fmt.Errorf("%w (run: %s)", ErrNotLoggedIn, s.loginHint())
		}
		var refreshErr error
		result, refreshErr = s.refreshLocked(ctx, false)
		return refreshErr
	})
	return result, err
}

func (s *Source) reloadLocked() error {
	path := s.path()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.cred = File{}
			s.loaded = true
			s.mtime = time.Time{}
			s.syncTerminalInvalidFromDisk()
			return nil
		}
		return err
	}
	if s.loaded && fi.ModTime().Equal(s.mtime) {
		s.syncTerminalInvalidFromDisk()
		return nil
	}
	cred, err := readFile(path)
	if err != nil {
		return err
	}
	s.cred = cred
	s.mtime = fi.ModTime()
	s.loaded = true
	s.syncTerminalInvalidFromDisk()
	return nil
}

// Save writes tokens for account under dir. Used by `prism auth xai` / `prism auth google`.
// The token write and the .invalid removal run under the SAME flock as the
// server's refreshes: a refresh in flight when the login completes must
// finish first, otherwise it would either overwrite the fresh login with
// its stale session's rotation or re-write .invalid on top of the new
// state — silently voiding the login.
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
	lock, err := lockFile(filepath.Join(dir, account+".json.lock"))
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	if err := writeFile(filepath.Join(dir, account+".json"), File{
		Provider:     provider,
		AccessToken:  tok.Access,
		RefreshToken: tok.Refresh,
		ExpiresAt:    tok.ExpiresAt,
	}); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, account+".json.invalid")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
		// The rename above already replaced the token file with the NEW
		// token: removing it here (the old behavior) would destroy a live
		// credential. Keep the file and surface the error — the refresh
		// path adopts the token in memory regardless, and Save reports the
		// login failure.
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
