package microvm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

// countingServer answers the first failUntil-1 requests with status, then 200.
func countingServer(t *testing.T, status, failUntil int, attempts *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if int(n) < failUntil {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	srv := countingServer(t, http.StatusServiceUnavailable, 3, &attempts)
	c := microvm.New(srv.URL, microvm.WithMaxRetries(3))

	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health should have succeeded after retries: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3 (two 503s then success)", got)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := countingServer(t, http.StatusServiceUnavailable, 99, &attempts) // never succeeds
	c := microvm.New(srv.URL, microvm.WithMaxRetries(2))

	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("Health should have failed after exhausting retries")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3 (1 try + 2 retries)", got)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32
	srv := countingServer(t, http.StatusBadRequest, 99, &attempts)
	c := microvm.New(srv.URL, microvm.WithMaxRetries(3))

	// A 400 is the request's own fault; retrying it just repeats the mistake.
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1 (a 400 is not retried)", got)
	}
}

func TestDoesNotRetryUnkeyedPost(t *testing.T) {
	var attempts atomic.Int32
	srv := countingServer(t, http.StatusServiceUnavailable, 99, &attempts)
	c := microvm.New(srv.URL, microvm.WithMaxRetries(3))

	// A POST with no idempotency key must not be retried: the first try may have
	// run the work even though the reply never arrived.
	_, err := c.Tasks.Create(context.Background(), microvm.TaskCreateParams{Image: "python", Cmd: "python3"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1 (an unkeyed POST is not retried)", got)
	}
}

func TestRetriesKeyedPost(t *testing.T) {
	var attempts atomic.Int32
	srv := countingServer(t, http.StatusServiceUnavailable, 99, &attempts)
	c := microvm.New(srv.URL, microvm.WithMaxRetries(2))

	// With a key the server can recognise the repeat, so the retry is safe.
	_, err := c.Tasks.Create(context.Background(),
		microvm.TaskCreateParams{Image: "python", Cmd: "python3"},
		microvm.WithIdempotencyKey("k1"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3 (a keyed POST is retried)", got)
	}
}

func TestObserverSeesEveryAttempt(t *testing.T) {
	var attempts atomic.Int32
	srv := countingServer(t, http.StatusBadGateway, 3, &attempts)

	var observed []microvm.RequestInfo
	c := microvm.New(srv.URL,
		microvm.WithMaxRetries(3),
		microvm.WithObserver(func(info microvm.RequestInfo) {
			observed = append(observed, info)
		}))

	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(observed) != 3 {
		t.Fatalf("observer saw %d attempts, want 3", len(observed))
	}
	for i, info := range observed {
		if info.Attempt != i+1 {
			t.Errorf("attempt %d reported as %d", i+1, info.Attempt)
		}
	}
	if observed[2].Status != http.StatusOK {
		t.Errorf("final attempt status = %d, want 200", observed[2].Status)
	}
}

func TestPtrHelper(t *testing.T) {
	p := microvm.Ptr(7)
	if p == nil || *p != 7 {
		t.Fatal("Ptr did not box the value")
	}
}
