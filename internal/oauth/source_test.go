package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/oauth/xai"
)

func TestSaveAndTokenBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().Add(time.Hour)
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "acc", Refresh: "ref", ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	src := NewXAISource(dir, "supergrok", func(context.Context, string) (xai.Tokens, error) {
		t.Fatal("refresh must not run before expiry")
		return xai.Tokens{}, nil
	})
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "acc" {
		t.Fatalf("token = %q", tok)
	}
	fi, err := os.Stat(filepath.Join(dir, "supergrok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", fi.Mode().Perm())
	}
}

func TestTokenRefreshesWhenExpired(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	src := NewXAISource(dir, "supergrok", func(_ context.Context, refresh string) (xai.Tokens, error) {
		n++
		if refresh != "ref-old" {
			t.Errorf("refresh = %q", refresh)
		}
		return xai.Tokens{
			Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour),
		}, nil
	})
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new" || n != 1 {
		t.Fatalf("token=%q refreshCalls=%d", tok, n)
	}
	data, err := os.ReadFile(filepath.Join(dir, "supergrok.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "new" || f.RefreshToken != "ref-new" {
		t.Fatalf("saved = %+v", f)
	}
}

func TestTokenNotLoggedIn(t *testing.T) {
	src := NewXAISource(t.TempDir(), "supergrok", nil)
	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("expected not logged in")
	}
}

func TestTokenPicksUpExternalSave(t *testing.T) {
	dir := t.TempDir()
	src := NewXAISource(dir, "supergrok", nil)
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected not logged in")
	}
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "from-cli", Refresh: "r", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "from-cli" {
		t.Fatalf("token = %q", tok)
	}
}

func TestSaveChownsToDirOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown to another user")
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("nobody user missing")
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.Chown(parent, uid, gid); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "oauth")
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "acc", Refresh: "ref", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "supergrok.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("stat sys")
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Fatalf("owner %d:%d, want %d:%d", st.Uid, st.Gid, uid, gid)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", fi.Mode().Perm())
	}
}

// --- xAI OAuth rework round 2: audit items ---

// Audit item 6a: an existing deployment has a token file but NO .lock
// (the lock file predates this feature). The first refresh must bootstrap
// the lock via O_CREATE instead of failing every refresh.
func TestTokenRefreshBootstrapsMissingLockFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "supergrok.json")
	// writeFile (not Save): Save now creates the .lock itself, which
	// would break the legacy-deployment precondition.
	if err := writeFile(tokenPath, File{
		Provider: "xai", AccessToken: "old", RefreshToken: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("precondition: .lock must not exist, stat err = %v", err)
	}
	n := 0
	src := NewXAISource(dir, "supergrok", func(_ context.Context, refresh string) (xai.Tokens, error) {
		n++
		if refresh != "ref-old" {
			t.Errorf("refresh = %q, want ref-old", refresh)
		}
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("refresh on a pre-lock deployment must succeed (O_CREATE), got: %v", err)
	}
	if tok != "new" || n != 1 {
		t.Fatalf("token=%q refreshCalls=%d", tok, n)
	}
	if fi, err := os.Stat(tokenPath + ".lock"); err != nil {
		t.Fatalf(".lock was not bootstrapped: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf(".lock perm = %o, want 0600", fi.Mode().Perm())
	}
}

// Audit item 6b: after a terminal refresh failure (invalid_grant), a
// re-login via Save must clear the in-memory latch WITHOUT a process
// restart. The one-way latch (only ever set, never cleared) used to keep
// intercepting every Token()/ForceRefresh call until the service
// restarted.
func TestTerminalThenReLoginRecoversWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-dead", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	src := NewXAISource(dir, "supergrok", func(_ context.Context, refresh string) (xai.Tokens, error) {
		n++
		return xai.Tokens{}, errors.New("xAI OAuth token refresh failed: invalid_grant Invalid or unknown refresh token")
	})
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
	if !src.OAuthTerminalInvalid() {
		t.Fatal("terminal latch must be set after invalid_grant")
	}
	if _, err := os.Stat(filepath.Join(dir, "supergrok.json.invalid")); err != nil {
		t.Fatalf(".invalid marker missing: %v", err)
	}
	// While terminal, Token() must fail fast without re-hitting the
	// refresher.
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected terminal error")
	}
	if n != 1 {
		t.Fatalf("terminal Token() must not call the refresher, calls = %d", n)
	}
	// Re-login: Save writes a fresh token and removes the marker — no
	// process restart.
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "fresh", Refresh: "ref-fresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("re-login must recover without a restart, got: %v", err)
	}
	if tok != "fresh" {
		t.Fatalf("token = %q, want fresh", tok)
	}
	if n != 1 {
		t.Fatalf("recovery must not call the refresher, calls = %d", n)
	}
	if src.OAuthTerminalInvalid() {
		t.Fatal("terminal latch must be cleared after re-login")
	}
}

// Audit item 7 (unit): N concurrent 401 handlers with the same rejected
// token must burn exactly ONE refresh-token rotation; the losers reuse
// the winner's rotated token from disk.
func TestRefreshIfStaleHerdBurnsOneRotation(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "stale", Refresh: "ref-1", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	src := NewXAISource(dir, "supergrok", func(_ context.Context, refresh string) (xai.Tokens, error) {
		if got := n.Add(1); got != 1 {
			t.Errorf("refresh called %d times — the herd must burn exactly one rotation", got)
			return xai.Tokens{}, errors.New("unexpected second rotation")
		}
		if refresh != "ref-1" {
			t.Errorf("refresh = %q, want ref-1", refresh)
		}
		return xai.Tokens{Access: "rotated", Refresh: "ref-2", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	const workers = 5
	results := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = src.RefreshIfStale(context.Background(), "stale")
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i] != "rotated" {
			t.Fatalf("worker %d: token = %q, want rotated", i, results[i])
		}
	}
	if n.Load() != 1 {
		t.Fatalf("rotations = %d, want exactly 1", n.Load())
	}
}

// Audit item 7 (unit, sequential): when the on-disk token is already
// newer than the rejected one (another actor rotated), RefreshIfStale
// must reuse it and not call the refresher at all.
func TestRefreshIfStaleSkipsWhenDiskAlreadyRotated(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "a", Refresh: "ref-a", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a concurrent actor (keepalive / another 401 handler) that
	// already rotated the token.
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "b", Refresh: "ref-b", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	src := NewXAISource(dir, "supergrok", func(_ context.Context, _ string) (xai.Tokens, error) {
		n++
		t.Error("refresh must not run: the on-disk token is already newer than the rejected one")
		return xai.Tokens{}, nil
	})
	tok, err := src.RefreshIfStale(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "b" || n != 0 {
		t.Fatalf("token=%q refreshCalls=%d, want b/0", tok, n)
	}
}

// Audit item 4: the rotation consumes the refresh token; when the persist
// fails, the new pair must be adopted in memory (the process keeps
// working) and the error must not be fatal to the request path. Once the
// disk is healthy again, the next refresh re-persists the current pair.
func TestRefreshKeepsNewTokenInMemoryWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	cur := "ref-old"
	src := NewXAISource(dir, "supergrok", func(_ context.Context, refresh string) (xai.Tokens, error) {
		if refresh != cur {
			t.Fatalf("refresh = %q, want %q (the consumed token must never be retried)", refresh, cur)
		}
		switch refresh {
		case "ref-old":
			cur = "ref-new"
			return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
		case "ref-new":
			cur = "ref-new2"
			return xai.Tokens{Access: "new2", Refresh: "ref-new2", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}
		t.Fatalf("unexpected refresh token %q", refresh)
		return xai.Tokens{}, nil
	})
	orig := writeFileFn
	writeFileFn = func(string, File) error { return errors.New("simulated persist failure (disk full)") }
	t.Cleanup(func() { writeFileFn = orig })

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("persist failure must not be fatal to the request path: %v", err)
	}
	if tok != "new" {
		t.Fatalf("token = %q, want new", tok)
	}
	// The disk still holds the OLD pair; the in-memory pair must be
	// served without another (pointless) refresh.
	var f File
	data, rerr := os.ReadFile(filepath.Join(dir, "supergrok.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "old" {
		t.Fatalf("disk should still hold the old token, got %q", f.AccessToken)
	}
	tok2, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != "new" {
		t.Fatalf("second token = %q, want new (in-memory adoption)", tok2)
	}
	// The lock-guarded paths (keepalive ForceRefresh) must not
	// resurrect the consumed old refresh token from disk either.
	writeFileFn = orig
	tok3, err := src.ForceRefresh(context.Background())
	if err != nil {
		t.Fatalf("persist recovery: %v", err)
	}
	if tok3 != "new2" {
		t.Fatalf("recovered token = %q, want new2", tok3)
	}
	data, rerr = os.ReadFile(filepath.Join(dir, "supergrok.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "new2" || f.RefreshToken != "ref-new2" {
		t.Fatalf("disk should hold the re-persisted pair, got %+v", f)
	}
}

// Audit item 5: a refresh in flight (holding the flock) when a login
// completes must finish BEFORE Save's token write + .invalid removal —
// otherwise the refresh would re-write .invalid on top of the fresh login
// (or overwrite the login with its stale session's rotation), silently
// voiding the login.
func TestSaveWaitsForInFlightRefreshLock(t *testing.T) {
	dir := t.TempDir()
	invalidPath := filepath.Join(dir, "supergrok.json.invalid")
	// Terminal state: old token + .invalid marker.
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidPath, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate the in-flight refresh: hold the flock, then finish by
	// re-writing the terminal marker (as a failed refresh would).
	lock, err := lockFile(filepath.Join(dir, "supergrok.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(invalidPath, []byte("invalid\n"), 0o600)
		unlockFile(lock)
	}()
	// Save must block until the in-flight refresh finishes; its state
	// (fresh token, no marker) must be the final one.
	if err := Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "fresh", Refresh: "ref-fresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	var f File
	data, rerr := os.ReadFile(filepath.Join(dir, "supergrok.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "fresh" {
		t.Fatalf("login token overwritten by the in-flight refresh: %q", f.AccessToken)
	}
	if _, err := os.Stat(invalidPath); !os.IsNotExist(err) {
		t.Fatal("in-flight refresh re-wrote .invalid after the login — the login is voided")
	}
}
