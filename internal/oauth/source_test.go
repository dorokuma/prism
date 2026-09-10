package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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

func writeAgyTokenFile(t *testing.T, path, access, refresh string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"auth_method": "consumer",
		"id_token":    "keep-this-id-token",
		"token": map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    "Bearer",
			"expiry":        time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGoogleRefreshAdoptsAgyRefreshToken(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-prism", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	agyPath := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	writeAgyTokenFile(t, agyPath, "agy-acc", "ref-agy")
	got := ""
	src := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		got = refresh
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	src.SetAgyTokenPath(agyPath)
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new" {
		t.Fatalf("token = %q", tok)
	}
	if got != "ref-agy" {
		t.Fatalf("used refresh = %q, want ref-agy (agy is the authority)", got)
	}
}

func TestGoogleRefreshAdoptsDifferentRTIgnoringExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-prism", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	got := ""
	src := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		got = refresh
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) {
		return xai.Tokens{
			Refresh:   "ref-agy-relogin",
			ExpiresAt: time.Now().Add(-time.Hour), // older expiry must not block adopt
		}, nil
	}
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "ref-agy-relogin" {
		t.Fatalf("used refresh = %q, want ref-agy-relogin (RT different = re-login)", got)
	}
}

func TestGoogleRefreshDualWritesAgyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	agyPath := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	writeAgyTokenFile(t, agyPath, "old-acc", "ref-old")
	src := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		if refresh != "ref-old" {
			t.Errorf("refresh = %q", refresh)
		}
		return xai.Tokens{Access: "new-acc", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	src.SetAgyTokenPath(agyPath)
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new-acc" {
		t.Fatalf("token = %q", tok)
	}
	raw, err := os.ReadFile(agyPath)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["auth_method"] != "consumer" {
		t.Fatalf("auth_method = %v", envelope["auth_method"])
	}
	if envelope["id_token"] != "keep-this-id-token" {
		t.Fatalf("id_token lost: %v", envelope["id_token"])
	}
	nested, ok := envelope["token"].(map[string]any)
	if !ok {
		t.Fatalf("nested token missing: %s", raw)
	}
	if nested["access_token"] != "new-acc" || nested["refresh_token"] != "ref-new" {
		t.Fatalf("agy token = %+v", nested)
	}
	if nested["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v", nested["token_type"])
	}
	data, err := os.ReadFile(filepath.Join(dir, "Gemini.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "new-acc" || f.RefreshToken != "ref-new" {
		t.Fatalf("prism copy = %+v", f)
	}
}

func TestGoogleRefreshAgyUnwritableDegrades(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	agyPath := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	writeAgyTokenFile(t, agyPath, "old-acc", "ref-old")
	origAgy, err := os.ReadFile(agyPath)
	if err != nil {
		t.Fatal(err)
	}
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	src.SetAgyTokenPath(agyPath)
	src.storeAgy = func(string, xai.Tokens) error {
		return errors.New("permission denied")
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unwritable agy must not fail the refresh: %v", err)
	}
	if tok != "new" {
		t.Fatalf("token = %q", tok)
	}
	after, err := os.ReadFile(agyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(origAgy) {
		t.Fatalf("agy file mutated on unwritable degrade")
	}
	data, err := os.ReadFile(filepath.Join(dir, "Gemini.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "new" || f.RefreshToken != "ref-new" {
		t.Fatalf("prism copy not updated: %+v", f)
	}
}

func TestGoogleInvalidGrantTriesBothRTsThenBackoff(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-prism", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	var nowMu sync.Mutex
	now := time.Now()
	var calls []string
	src := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		calls = append(calls, refresh)
		return xai.Tokens{}, errors.New("Google OAuth token refresh failed: invalid_grant: Token has been expired or revoked.")
	})
	src.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) {
		return xai.Tokens{Refresh: "ref-agy"}, nil
	}
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("expected invalid_grant")
	}
	if src.OAuthTerminalInvalid() {
		t.Fatal("google invalid_grant must not set the permanent terminal latch")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "Gemini.json.invalid")); !os.IsNotExist(statErr) {
		t.Fatalf(".invalid marker must not be written for google, stat err = %v", statErr)
	}
	if len(calls) != 2 || calls[0] != "ref-agy" || calls[1] != "ref-prism" {
		t.Fatalf("calls = %v, want [ref-agy ref-prism] (agy then prism)", calls)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err = %v, want invalid_grant", err)
	}
	_, err2 := src.Token(context.Background())
	if err2 == nil || !strings.Contains(err2.Error(), "backoff") {
		t.Fatalf("in-backoff Token must fail fast, err = %v", err2)
	}
	if len(calls) != 2 {
		t.Fatalf("backoff must not refresh again, calls = %d", len(calls))
	}
}

func TestGoogleInvalidGrantRetrySucceedsWithoutLatch(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-prism", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	nLoad := 0
	src := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		if refresh == "ref-prism" {
			return xai.Tokens{}, errors.New("invalid_grant")
		}
		if refresh == "ref-agy" {
			return xai.Tokens{Access: "recovered", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}
		return xai.Tokens{}, errors.New("unexpected refresh " + refresh)
	})
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) {
		nLoad++
		if nLoad == 1 {
			return xai.Tokens{Refresh: "ref-prism"}, nil
		}
		return xai.Tokens{Refresh: "ref-agy"}, nil
	}
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "recovered" {
		t.Fatalf("token = %q", tok)
	}
	if src.OAuthTerminalInvalid() {
		t.Fatal("successful retry must not latch")
	}
}

func TestGoogleInvalidGrantSameRTBacksOffWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-same", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		n++
		return xai.Tokens{}, errors.New("invalid_grant")
	})
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) {
		return xai.Tokens{Refresh: "ref-same"}, nil
	}
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("expected invalid_grant")
	}
	if n != 1 {
		t.Fatalf("refresh calls = %d, want 1 (same RT, no retry)", n)
	}
	if src.OAuthTerminalInvalid() {
		t.Fatal("google same-RT invalid_grant must backoff, not terminal-latch")
	}
}

func TestNewSourceDoesNotBindCanonicalAgyPath(t *testing.T) {
	src := NewSource(t.TempDir(), "Gemini", "google", nil)
	if src.agyPath != "" {
		t.Fatalf("agyPath = %q, want empty (NewSource must not touch the live file)", src.agyPath)
	}
	if src.loadAgy != nil || src.storeAgy != nil {
		t.Fatal("agy helpers must be nil until SetAgyTokenPath")
	}
}

func TestGoogleAgyPathIsolatedAcrossAccounts(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old-a", Refresh: "ref-a", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, "Gemini2", "google", xai.Tokens{
		Access: "old-b", Refresh: "ref-b", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(t.TempDir(), "agy-a")
	writeAgyTokenFile(t, pathA, "agy-acc", "ref-agy-a")
	var callsA, callsB []string
	srcA := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		callsA = append(callsA, refresh)
		return xai.Tokens{Access: "new-a", Refresh: "ref-a", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	srcB := NewSource(dir, "Gemini2", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		callsB = append(callsB, refresh)
		return xai.Tokens{Access: "new-b", Refresh: "ref-b", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	srcA.SetAgyTokenPath(pathA)
	// srcB deliberately unbound — must not see pathA.
	tokA, err := srcA.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := srcB.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tokA != "new-a" || tokB != "new-b" {
		t.Fatalf("tokens a=%q b=%q", tokA, tokB)
	}
	if len(callsA) != 1 || callsA[0] != "ref-agy-a" {
		t.Fatalf("account A calls = %v, want [ref-agy-a]", callsA)
	}
	if len(callsB) != 1 || callsB[0] != "ref-b" {
		t.Fatalf("account B calls = %v, want [ref-b] (must not adopt A's agy RT)", callsB)
	}
}

func TestGoogleBackoffClearedOnSuccessfulRefresh(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	var nowMu sync.Mutex
	now := time.Now()
	n := 0
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		n++
		if n == 1 {
			return xai.Tokens{}, errors.New("invalid_grant")
		}
		return xai.Tokens{Access: "recovered", Refresh: "ref", ExpiresAt: now.Add(time.Hour)}, nil
	})
	src.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) { return xai.Tokens{Refresh: "ref"}, nil }
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected first invalid_grant")
	}
	if n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	nowMu.Lock()
	now = now.Add(time.Minute)
	nowMu.Unlock()
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "recovered" {
		t.Fatalf("token = %q", tok)
	}
	if src.googleBackoffStep != 0 || !src.googleBackoffUntil.IsZero() {
		t.Fatalf("backoff not cleared after success: step=%d until=%s", src.googleBackoffStep, src.googleBackoffUntil)
	}
}

func TestGoogleBackoffClearedWhenAgyRTChanges(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	var nowMu sync.Mutex
	now := time.Now()
	agyRT := "ref-old"
	var agyMu sync.Mutex
	n := 0
	src := NewSource(dir, "Gemini", "google", func(_ context.Context, refresh string) (xai.Tokens, error) {
		n++
		if refresh == "ref-old" {
			return xai.Tokens{}, errors.New("invalid_grant")
		}
		if refresh == "ref-relogin" {
			return xai.Tokens{Access: "from-relogin", Refresh: "ref-relogin", ExpiresAt: now.Add(time.Hour)}, nil
		}
		return xai.Tokens{}, errors.New("unexpected refresh " + refresh)
	})
	src.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) {
		agyMu.Lock()
		defer agyMu.Unlock()
		return xai.Tokens{Refresh: agyRT}, nil
	}
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected invalid_grant")
	}
	if n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	// Still inside the 1m backoff window: changing agy RT must unlatch immediately.
	agyMu.Lock()
	agyRT = "ref-relogin"
	agyMu.Unlock()
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("agy RT change must clear backoff immediately: %v", err)
	}
	if tok != "from-relogin" {
		t.Fatalf("token = %q", tok)
	}
	if n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestGoogleBackoffEmptyBaselineDoesNotUnlatch(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	var nowMu sync.Mutex
	now := time.Now()
	var agyMu sync.Mutex
	agyFail := true
	n := 0
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		n++
		return xai.Tokens{}, errors.New("invalid_grant")
	})
	src.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) {
		agyMu.Lock()
		defer agyMu.Unlock()
		if agyFail {
			return xai.Tokens{}, errors.New("agy unavailable")
		}
		return xai.Tokens{Refresh: "ref"}, nil
	}
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected first invalid_grant")
	}
	if src.googleBackoffAgyRT != "" {
		t.Fatalf("baseline = %q, want empty after agy read failure", src.googleBackoffAgyRT)
	}
	if n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	// Agy read recovers with the same RT while still inside the 1m window.
	// Empty baseline must not be treated as re-login.
	agyMu.Lock()
	agyFail = false
	agyMu.Unlock()
	if _, err := src.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("empty baseline + recovered same RT must stay latched: %v", err)
	}
	if n != 1 {
		t.Fatalf("in-window refresh calls = %d, want 1", n)
	}
}

func TestGoogleBackoffExponentialThenCap(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	var nowMu sync.Mutex
	now := time.Now()
	n := 0
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		n++
		return xai.Tokens{}, errors.New("invalid_grant")
	})
	src.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	src.SetAgyTokenPath(filepath.Join(t.TempDir(), "agy"))
	src.loadAgy = func(string) (xai.Tokens, error) { return xai.Tokens{Refresh: "ref"}, nil }
	src.storeAgy = func(string, xai.Tokens) error { return nil }
	advance := func(d time.Duration) {
		nowMu.Lock()
		now = now.Add(d)
		nowMu.Unlock()
	}
	wantUntil := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, d := range wantUntil {
		if _, err := src.Token(context.Background()); err == nil {
			t.Fatalf("step %d: expected invalid_grant", i)
		}
		nowMu.Lock()
		until := src.googleBackoffUntil
		cur := now
		nowMu.Unlock()
		got := until.Sub(cur)
		if got != d {
			t.Fatalf("step %d: backoff = %s, want %s", i, got, d)
		}
		// Still inside the window: no extra refresh.
		calls := n
		if _, err := src.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "backoff") {
			t.Fatalf("step %d in-window: err = %v", i, err)
		}
		if n != calls {
			t.Fatalf("step %d in-window refreshed, calls %d → %d", i, calls, n)
		}
		advance(d)
	}
	if n != len(wantUntil) {
		t.Fatalf("refresh calls = %d, want %d", n, len(wantUntil))
	}
}

func TestGoogleAccessFreshnessUsesRefreshSkew(t *testing.T) {
	dir := t.TempDir()
	var nowMu sync.Mutex
	now := time.Now()
	realExp := now.Add(time.Hour)
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "acc", Refresh: "ref", ExpiresAt: realExp,
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		n++
		return xai.Tokens{Access: "refreshed", Refresh: "ref", ExpiresAt: now.Add(2 * time.Hour)}, nil
	})
	src.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "acc" || n != 0 {
		t.Fatalf("fresh token must not refresh, tok=%q n=%d", tok, n)
	}
	nowMu.Lock()
	now = now.Add(56 * time.Minute) // past 5m skew, still before real expiry
	nowMu.Unlock()
	tok, err = src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "refreshed" || n != 1 {
		t.Fatalf("after skew window must refresh, tok=%q n=%d", tok, n)
	}
}

func TestGooglePersistRefusesInvalidAgyJSON(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Gemini", "google", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	agyPath := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	orig := []byte("not-json{")
	if err := os.WriteFile(agyPath, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewSource(dir, "Gemini", "google", func(context.Context, string) (xai.Tokens, error) {
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	src.SetAgyTokenPath(agyPath) // real Load/Store helpers
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("invalid agy JSON must not fail the refresh: %v", err)
	}
	if tok != "new" {
		t.Fatalf("token = %q", tok)
	}
	after, err := os.ReadFile(agyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(orig) {
		t.Fatalf("agy file overwritten on parse failure: %q", after)
	}
	data, err := os.ReadFile(filepath.Join(dir, "Gemini.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "new" || f.RefreshToken != "ref-new" {
		t.Fatalf("prism copy not updated: %+v", f)
	}
}
