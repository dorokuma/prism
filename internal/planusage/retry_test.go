package planusage

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type retryFetcher struct {
	hits atomic.Int32
	fail int32
}

func (f *retryFetcher) Match(string, string) bool { return true }
func (f *retryFetcher) Fetch(context.Context, AccountView) (Snapshot, error) {
	if f.hits.Add(1) <= f.fail {
		return Snapshot{Provider: "test"}, errors.New("temporary network failure")
	}
	return Snapshot{Provider: "test", Windows: []Window{{Name: "weekly"}}}, nil
}

type retryAccount struct{}

func (retryAccount) Name() string         { return "test" }
func (retryAccount) Provider() string     { return "test" }
func (retryAccount) BaseURL() string      { return "https://example.invalid" }
func (retryAccount) Key() string          { return "key" }
func (retryAccount) AuthHeader() string   { return "" }
func (retryAccount) Client() *http.Client { return nil }

func TestFetchWithRetryRecoversTransientFailure(t *testing.T) {
	f := &retryFetcher{fail: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snap, err := FetchWithRetry(ctx, f, retryAccount{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("FetchWithRetry() error = %v", err)
	}
	if f.hits.Load() != 3 || len(snap.Windows) != 1 {
		t.Fatalf("hits=%d snapshot=%+v, want 3 attempts and a window", f.hits.Load(), snap)
	}
}

func TestFetchWithRetryGivesUpAfterThreeAttempts(t *testing.T) {
	f := &retryFetcher{fail: 10}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := FetchWithRetry(ctx, f, retryAccount{}, 20*time.Millisecond)
	if err == nil || f.hits.Load() != 3 {
		t.Fatalf("error=%v hits=%d, want error after 3 attempts", err, f.hits.Load())
	}
}

type nonRetryFetcher struct {
	hits atomic.Int32
	err  error
}

func (f *nonRetryFetcher) Match(string, string) bool { return true }
func (f *nonRetryFetcher) Fetch(context.Context, AccountView) (Snapshot, error) {
	f.hits.Add(1)
	return Snapshot{Provider: "test"}, f.err
}

func TestFetchWithRetryDoesNotRetryNonRetryableError(t *testing.T) {
	for _, tc := range []error{ErrUnauthorized, ErrNoSubscription} {
		f := &nonRetryFetcher{err: tc}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := FetchWithRetry(ctx, f, retryAccount{}, 20*time.Millisecond)
		cancel()
		if !errors.Is(err, tc) || f.hits.Load() != 1 {
			t.Fatalf("err=%v got=%v hits=%d, want original error after 1 attempt", tc, err, f.hits.Load())
		}
	}
}
