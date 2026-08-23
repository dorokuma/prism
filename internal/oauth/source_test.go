package oauth

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
