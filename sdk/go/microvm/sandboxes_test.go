package microvm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

// The tag filter is the one place the SDK has to build a wire format rather than
// pass one through: the server splits each value at its first colon, and repeats
// AND. A map rendered any other way is a filter that silently matches nothing.
func TestSandboxListEncodesTagsAsKeyColonValue(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		writeJSON(w, map[string]any{
			"object": "list", "url": "/v1/sandboxes", "has_more": false,
			"data": []map[string]any{},
		})
	}))
	t.Cleanup(srv.Close)

	client := microvm.New(srv.URL, microvm.WithToken("t"))
	_, err := client.Sandboxes.List(context.Background(), microvm.SandboxListParams{
		State: microvm.SandboxStateRunning,
		// Two of them, out of key order, and a value carrying a colon of its own.
		Tags: map[string]string{"owner": "me", "env": "ci", "url": "https://x:8443"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Sorted by key, so the same filter always produces the same URL.
	const want = "state=running&tag=env%3Aci&tag=owner%3Ame&tag=url%3Ahttps%3A%2F%2Fx%3A8443"
	if query != want {
		t.Errorf("query = %s\nwant      %s", query, want)
	}
}

// No tags means no tag parameter at all, rather than an empty one the server
// would have to refuse.
func TestSandboxListOmitsAnEmptyTagFilter(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		writeJSON(w, map[string]any{
			"object": "list", "url": "/v1/sandboxes", "has_more": false,
			"data": []map[string]any{},
		})
	}))
	t.Cleanup(srv.Close)

	client := microvm.New(srv.URL, microvm.WithToken("t"))
	if _, err := client.Sandboxes.List(context.Background(), microvm.SandboxListParams{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if query != "limit=10" {
		t.Errorf("query = %q, want limit=10", query)
	}
}

// Every route can answer 429 now that a token may carry a rate, and the four
// routes below take no Idempotency-Key -- so without being marked replayable they
// are the ones where a rate limit surfaces to user code as a hard failure. They
// are safe to repeat by construction: extending never brings a deadline forward, a
// write is an overwrite, and mkdir -p on an existing directory is success.
func TestRoutesThatAreIdempotentByDesignAreRetried(t *testing.T) {
	tests := []struct {
		name string
		call func(*microvm.Client) error
	}{
		{"extend", func(c *microvm.Client) error {
			_, err := c.Sandboxes.Extend(context.Background(), "sb_1", 600)
			return err
		}},
		{"write a file", func(c *microvm.Client) error {
			_, err := c.Files.Write(context.Background(), "sb_1", "/app/main.py", []byte("x = 1\n"))
			return err
		}},
		{"write a batch", func(c *microvm.Client) error {
			_, err := c.Files.CreateBatch(context.Background(), "sb_1",
				[]microvm.FileCreateParams{{Path: "/app/main.py", Content: []byte("x = 1\n")}})
			return err
		}},
		{"make a directory", func(c *microvm.Client) error {
			_, err := c.Files.Mkdir(context.Background(), "sb_1", "/app/out")
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := countingServer(t, http.StatusTooManyRequests, 99, &attempts)
			c := microvm.New(srv.URL, microvm.WithMaxRetries(2))

			if err := tc.call(c); err == nil {
				t.Fatal("expected an error")
			}
			if got := attempts.Load(); got != 3 {
				t.Errorf("made %d attempts, want 3: a 429 on this route reached user code unretried", got)
			}
		})
	}
}
