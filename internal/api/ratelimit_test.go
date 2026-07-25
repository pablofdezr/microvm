package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/auth"
	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// clock is a test's own idea of now, moved by hand. A rate limiter tested by
// sleeping is a test that is either slow or flaky, and this one has to assert
// what happens a second from now.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}

func (c *clock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *clock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// limitHarness is a server whose tokens carry per-tenant limits, which is the
// minimum needed to show that one tenant's limit is not another's.
type limitHarness struct {
	srv *httptest.Server
	api *Server
	clk *clock
}

func newLimitHarness(t *testing.T, principals map[string]*auth.Principal) *limitHarness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := sandbox.NewManager(runtimetest.New(), logstore.New(logstore.Config{}), log)
	api := NewServer(Config{Principals: principals, Images: []string{"python"}}, mgr, nil, nil, log)

	clk := newClock()
	api.limiter.now = clk.now

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = mgr.Close(t.Context())
	})
	return &limitHarness{srv: srv, api: api, clk: clk}
}

// do sends one request as the given token. The body is raw so a test says what
// went over the wire rather than what a struct serialised to.
func (h *limitHarness) do(t *testing.T, token, method, path, body string) *http.Response {
	t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// --- the reply --------------------------------------------------------------

// A refused request has to arrive in the envelope every other error uses, with
// the type all three SDKs already back off on, and it has to say when to come
// back: a caller told only "no" retries immediately, which turns one flood into
// two.
func TestRateLimitedRequestIsEnvelopedWithRetryAfter(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_a": {MaxRequestsPerSecond: 1},
	})

	if got := h.do(t, "sk_a", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
		t.Fatalf("the first request was refused: %d", got)
	}

	resp := h.do(t, "sk_a", "GET", "/v1/sandboxes", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Type != apitypes.ErrorTypeCapacityError {
		t.Errorf("type = %q, want capacity_error: an SDK cannot tell to back off", env.Error.Type)
	}
	if env.Error.Code != CodeNodeAtCapacity {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNodeAtCapacity)
	}
	if env.Error.Message == "" {
		t.Error("message is empty; a caller has nothing to read")
	}
	if env.Error.RequestId == nil || *env.Error.RequestId == "" {
		t.Error("no request_id: an error a caller cannot quote is an error we cannot find")
	}

	retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, want seconds: %v", resp.Header.Get("Retry-After"), err)
	}
	if retryAfter != 1 {
		t.Errorf("Retry-After = %d, want 1 at one request per second", retryAfter)
	}
}

// Retry-After has to follow the configured rate rather than be a constant, or a
// tightly limited caller is sent back before there is anything for them.
func TestRetryAfterFollowsTheRate(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_slow": {MaxRequestsPerSecond: 0.5},
	})

	h.do(t, "sk_slow", "GET", "/v1/sandboxes", "")
	resp := h.do(t, "sk_slow", "GET", "/v1/sandboxes", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 at half a request per second", got)
	}
}

// The wait has to be honest: what Retry-After promised must actually work.
func TestTheAllowanceRefills(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_a": {MaxRequestsPerSecond: 1},
	})

	h.do(t, "sk_a", "GET", "/v1/sandboxes", "")
	if got := h.do(t, "sk_a", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", got)
	}

	h.clk.advance(time.Second)
	if got := h.do(t, "sk_a", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
		t.Errorf("status = %d after waiting the second Retry-After asked for, want 200", got)
	}
}

// --- one tenant is not another ----------------------------------------------

// The property the whole feature rests on: a tenant spending its allowance costs
// nobody else anything.
func TestOneTenantsRateLimitIsNotAnothers(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_a": {MaxRequestsPerSecond: 1},
		"sk_b": {MaxRequestsPerSecond: 1},
	})

	h.do(t, "sk_a", "GET", "/v1/sandboxes", "")
	if got := h.do(t, "sk_a", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("tenant a: status = %d, want 429", got)
	}

	if got := h.do(t, "sk_b", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
		t.Errorf("tenant b: status = %d, want 200 -- one tenant starved another", got)
	}
}

// Keyed on the tenant, not the token: two keys belonging to one tenant share one
// allowance, or the limit is a limit on how many keys a caller bothers to make.
func TestRotatingTokensDoesNotMultiplyTheAllowance(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_old": {Tenant: "acme", MaxRequestsPerSecond: 1},
		"sk_new": {Tenant: "acme", MaxRequestsPerSecond: 1},
	})

	if got := h.do(t, "sk_old", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
		t.Fatalf("the first request was refused: %d", got)
	}
	if got := h.do(t, "sk_new", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: a second key bought the same tenant a second allowance", got)
	}
}

// Zero is unlimited, and it is what every existing deployment has.
func TestNoRateConfiguredIsUnlimited(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{"sk_a": {}})

	for i := range 50 {
		if got := h.do(t, "sk_a", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
			t.Fatalf("request %d was refused with %d on an unlimited key", i, got)
		}
	}
	if n := h.api.limiter.size(); n != 0 {
		t.Errorf("an unlimited key allocated %d buckets, want 0", n)
	}
}

// --- bounded memory ---------------------------------------------------------

// A limiter whose map grows with the traffic it is refusing has replaced a load
// problem with a memory one.
func TestTheBucketMapIsBounded(t *testing.T) {
	l := newRateLimiter()
	clk := newClock()
	l.now = clk.now

	for i := range maxRateBuckets * 2 {
		l.allow("t_"+strconv.Itoa(i), 1000)
	}

	if n := l.size(); n > maxRateBuckets {
		t.Errorf("the map holds %d buckets, past the cap of %d", n, maxRateBuckets)
	}
}

// An idle bucket has refilled to full, so it says nothing a fresh one would not.
// Keeping it would make the map the size of every tenant that ever called.
func TestIdleBucketsAreEvicted(t *testing.T) {
	l := newRateLimiter()
	clk := newClock()
	l.now = clk.now

	l.allow("t_a", 1)
	l.allow("t_b", 1)
	if n := l.size(); n != 2 {
		t.Fatalf("size = %d, want 2", n)
	}

	clk.advance(rateBucketTTL + time.Minute)

	// The sweep runs when a bucket is created, so it takes an arrival to collect
	// the ones that went idle.
	l.allow("t_c", 1)
	if n := l.size(); n != 1 {
		t.Errorf("size = %d after two buckets went idle, want 1", n)
	}
}

// The limiter runs after auth, and this is why: an invalid token is refused
// before it can name a bucket, so a flood from strangers costs no memory. A
// limiter in front of auth could only key on what the caller sent.
func TestAnUnauthenticatedFloodAllocatesNoBuckets(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_a": {MaxRequestsPerSecond: 1},
	})

	for i := range 100 {
		resp := h.do(t, "sk_invalid_"+strconv.Itoa(i), "GET", "/v1/sandboxes", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	}

	if n := h.api.limiter.size(); n != 0 {
		t.Errorf("100 invalid tokens left %d buckets behind: the map is keyed on "+
			"something a stranger controls", n)
	}
}

// --- the sandbox cap through the API ----------------------------------------

// A caller at their own cap gets the same 429 and the same capacity_error as a
// full node, so no SDK's retry logic changes -- and a different message, because
// waiting for room that is already there is not the fix.
func TestTenantSandboxCapIsReportedAsCapacity(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_a": {MaxConcurrent: 1},
		"sk_b": {MaxConcurrent: 1},
	})

	first := h.do(t, "sk_a", "POST", "/v1/sandboxes", `{"image":"python"}`)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first sandbox: %d", first.StatusCode)
	}
	sb := decode[apitypes.Sandbox](t, first)

	resp := h.do(t, "sk_a", "POST", "/v1/sandboxes", `{"image":"python"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Type != apitypes.ErrorTypeCapacityError {
		t.Errorf("type = %q, want capacity_error", env.Error.Type)
	}
	if env.Error.Code != CodeNodeAtCapacity {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNodeAtCapacity)
	}
	// The message is the only thing that separates this from a full node, so it
	// has to point at the caller's own sandboxes.
	if !strings.Contains(env.Error.Message, "sandboxes running") {
		t.Errorf("message = %q, want it to say the caller is at their own limit", env.Error.Message)
	}

	// Another tenant is untouched.
	if got := h.do(t, "sk_b", "POST", "/v1/sandboxes", `{"image":"python"}`).StatusCode; got != http.StatusCreated {
		t.Errorf("tenant b: status = %d, want 201 -- one tenant's cap refused another's create", got)
	}

	// And the slot comes back on delete, rather than being held for the retention
	// window the stopped sandbox is still listed for.
	h.do(t, "sk_a", "DELETE", "/v1/sandboxes/"+sb.Id, "")
	if got := h.do(t, "sk_a", "POST", "/v1/sandboxes", `{"image":"python"}`).StatusCode; got != http.StatusCreated {
		t.Errorf("status = %d after deleting one, want 201", got)
	}
}

// Two keys of one tenant carrying different rates get their own buckets, because
// one bucket would take its burst and its refill from whichever key called last:
// a single request on a 100 rps key refilled the bucket a 1 rps key then drained,
// so the tighter limit was not enforced at all. Keys at the same rate still share
// one, which is the property the whole limit rests on.
func TestOneTenantsKeysAtDifferentRatesDoNotLendEachOtherAllowance(t *testing.T) {
	h := newLimitHarness(t, map[string]*auth.Principal{
		"sk_ops": {Tenant: "acme", MaxRequestsPerSecond: 100},
		"sk_ci":  {Tenant: "acme", MaxRequestsPerSecond: 1},
	})

	// The lax key first, so a shared bucket would be sitting at a burst of 100.
	if got := h.do(t, "sk_ops", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
		t.Fatalf("the ops key was refused: %d", got)
	}

	if got := h.do(t, "sk_ci", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
		t.Fatalf("the ci key's own first request was refused: %d", got)
	}
	if got := h.do(t, "sk_ci", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: a 1 rps key was spending a 100 rps key's allowance", got)
	}

	// And the tight key has not clamped the lax one either.
	for i := range 20 {
		if got := h.do(t, "sk_ops", "GET", "/v1/sandboxes", "").StatusCode; got != http.StatusOK {
			t.Fatalf("request %d on the 100 rps key was refused with %d: the 1 rps key clamped it", i, got)
		}
	}
}
