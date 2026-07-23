// Package microvm is the Go client for the microvm API.
//
// The resource types and their fields in types.gen.go are generated from
// api/openapi.yaml, the same file the server is generated from. What is written
// by hand is only what a generator does badly: the transport, typed errors,
// auto-pagination, streaming, and the handful of helpers that turn three calls
// into one.
//
//	client := microvm.New("http://127.0.0.1:8080", microvm.WithToken(token))
//
//	sb, err := client.Sandboxes.Create(ctx, microvm.SandboxCreateParams{
//	    Image: "python",
//	})
//	defer client.Sandboxes.Delete(ctx, sb.Id)
//
//	out, err := client.Run(ctx, sb.Id, "python3", "main.py")
//	fmt.Print(out.Stdout)
package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds an ordinary request.
//
// Streaming and long polls opt out: a stream that ends after 30 seconds because
// of a client-side timer is a stream that fails on any workload worth watching.
const DefaultTimeout = 30 * time.Second

// DefaultBaseURL is where a daemon listens unless told otherwise. It is
// loopback because the daemon's own default is, and for the same reason: this
// API creates VMs that run arbitrary code, so an open one is an open shell.
const DefaultBaseURL = "http://127.0.0.1:8080"

// Version is this SDK's version. It rides along in the User-Agent so a daemon's
// logs can tell one client generation from another when a call misbehaves.
const Version = "0.1.0"

// DefaultMaxRetries is how many times a transient failure is retried before it
// is returned. Only idempotent requests are retried (see WithMaxRetries).
const DefaultMaxRetries = 2

// Client talks to a microvm daemon.
//
// Resources hang off it by name rather than being methods on the client itself:
// client.Sandboxes.Create reads as what it does, where a flat CreateSandbox
// leaves a caller scanning one long list of methods to find out what exists.
type Client struct {
	baseURL    string
	token      string
	http       *http.Client
	maxRetries int
	observe    func(RequestInfo)

	Sandboxes  *SandboxService
	Executions *ExecutionService
	Files      *FileService
	Tasks      *TaskService
	Queue      *QueueService
	Images     *ImageService
	Tenants    *TenantService
}

// Option configures a Client.
type Option func(*Client)

// WithToken sets the bearer token.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithHTTPClient supplies the underlying client, for callers who need their own
// transport, proxy or TLS configuration.
//
// Its Timeout is ignored for streams and long polls, which manage their own
// deadlines through the context.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithMaxRetries sets how many times a transient failure -- a network error, or
// a 429/500/502/503/504 -- is retried before it is returned, with exponential
// backoff and jitter between tries and any Retry-After header honoured.
//
// Only idempotent requests are retried: GET, PUT and DELETE always, and POST
// only when it carries an idempotency key, since retrying a create without one
// could run the work twice. Zero disables retries.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// RequestInfo is what an observer is told about one HTTP attempt.
type RequestInfo struct {
	Method   string
	Path     string
	Attempt  int // 1 for the first try, 2 for the first retry, ...
	Status   int // 0 when the request never got a response
	Err      error
	Duration time.Duration
}

// WithObserver registers a function called once per HTTP attempt, for logging,
// metrics or tracing. It fires for retries too, so a caller can see a call that
// succeeded only on its third try. It must not block.
func WithObserver(fn func(RequestInfo)) Option {
	return func(c *Client) { c.observe = fn }
}

// New returns a client for a daemon.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		http:       &http.Client{Timeout: DefaultTimeout},
		maxRetries: DefaultMaxRetries,
	}
	for _, opt := range opts {
		opt(c)
	}

	c.Sandboxes = &SandboxService{c: c}
	c.Executions = &ExecutionService{c: c}
	c.Files = &FileService{c: c}
	c.Tasks = &TaskService{c: c}
	c.Queue = &QueueService{c: c}
	c.Images = &ImageService{c: c}
	c.Tenants = &TenantService{c: c}
	return c
}

// APIError is a failure the API reported.
//
// The fields are the API's, unchanged. Type is what to branch on -- it says
// what class of thing went wrong and therefore what to do -- while Code says
// exactly which thing, for the cases where that matters.
type APIError struct {
	StatusCode int
	Type       ErrorType
	Code       string
	Message    string
	Param      string
	RequestID  string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("microvm: %s (%s)", e.Message, e.Code)
	if e.Param != "" {
		msg += fmt.Sprintf(" [param: %s]", e.Param)
	}
	if e.RequestID != "" {
		msg += fmt.Sprintf(" [request: %s]", e.RequestID)
	}
	return msg
}

// IsNotFound reports whether err is an object that does not exist.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// IsCapacity reports whether err is the node having no room.
//
// This is the one error worth retrying as-is. It is also the signal to consider
// a task instead: tasks wait for a slot anywhere in the fleet rather than
// failing.
func IsCapacity(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Type == ErrorTypeCapacityError
}

// IsConflict reports whether err is an object in a state that forbids the call
// -- executing in a stopped sandbox, or reusing an idempotency key.
func IsConflict(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusConflict
}

// IsForbidden reports whether err is the key lacking permission -- an ordinary
// token calling an admin-only endpoint, such as setting a tenant's policy. It is
// distinct from a missing token: the request was authenticated, and refused.
func IsForbidden(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusForbidden
}

// requestOptions are the per-call knobs.
type requestOptions struct {
	idempotencyKey string
	query          string
}

// RequestOption customises a single call.
type RequestOption func(*requestOptions)

// WithIdempotencyKey makes a create safe to retry.
//
// Worth using for anything whose repetition would cost money or have side
// effects. A request whose reply never arrived cannot be known to have failed,
// so a retry without a key may run the work twice; with one, the retry returns
// the original answer.
func WithIdempotencyKey(key string) RequestOption {
	return func(o *requestOptions) { o.idempotencyKey = key }
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, opts ...RequestOption) error {
	resp, err := c.raw(ctx, method, path, body, opts...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("microvm: decoding the reply to %s %s: %w", method, path, err)
	}
	return nil
}

// raw performs a request and turns a non-2xx into an *APIError. The caller closes
// the body. Transient failures are retried per the client's retry policy.
func (c *Client) raw(ctx context.Context, method, path string, body any, opts ...RequestOption) (*http.Response, error) {
	var o requestOptions
	for _, opt := range opts {
		opt(&o)
	}

	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("microvm: encoding the request to %s %s: %w", method, path, err)
		}
		bodyBytes = b
	}

	url := c.baseURL + "/v1" + path
	if o.query != "" {
		url += "?" + o.query
	}

	// Retrying a request that is not idempotent could run the work twice. GET,
	// PUT and DELETE are safe by definition; a POST is only safe with a key that
	// lets the server recognise the repeat.
	idempotent := method == http.MethodGet || method == http.MethodHead ||
		method == http.MethodPut || method == http.MethodDelete ||
		o.idempotencyKey != ""

	var lastErr error
	for attempt := 1; attempt <= c.maxRetries+1; attempt++ {
		var rdr io.Reader
		if bodyBytes != nil {
			rdr = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "microvm-go/"+Version)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if o.idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", o.idempotencyKey)
		}

		start := time.Now()
		resp, err := c.http.Do(req)
		if c.observe != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			c.observe(RequestInfo{Method: method, Path: path, Attempt: attempt, Status: status, Err: err, Duration: time.Since(start)})
		}

		if err != nil {
			lastErr = fmt.Errorf("microvm: %s %s: %w", method, path, err)
			if idempotent && attempt <= c.maxRetries && ctx.Err() == nil {
				if werr := backoff(ctx, attempt, ""); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode >= 400 {
			if retryableStatus(resp.StatusCode) && idempotent && attempt <= c.maxRetries {
				retryAfter := resp.Header.Get("Retry-After")
				drainClose(resp)
				if werr := backoff(ctx, attempt, retryAfter); werr != nil {
					return nil, werr
				}
				continue
			}
			defer resp.Body.Close()
			return nil, parseError(resp)
		}
		return resp, nil
	}
	return nil, lastErr
}

// retryableStatus reports whether a status is worth trying again: the server is
// overloaded (429) or momentarily broken (5xx), not the request being wrong.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff waits before the next attempt: the server's Retry-After if it gave
// one, otherwise exponential backoff with full jitter. It returns early if the
// context is cancelled, so a retry loop never outlives its caller.
func backoff(ctx context.Context, attempt int, retryAfter string) error {
	const base = 100 * time.Millisecond
	const maxWait = 3 * time.Second

	wait := parseRetryAfter(retryAfter)
	if wait <= 0 {
		// Exponential: base, 2x, 4x, ... capped. Full jitter spreads a thundering
		// herd of clients that all failed at the same instant.
		ceil := base << (attempt - 1)
		if ceil > maxWait || ceil <= 0 {
			ceil = maxWait
		}
		wait = time.Duration(rand.Int63n(int64(ceil) + 1))
	}
	if wait > maxWait {
		wait = maxWait
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parseRetryAfter reads the header in both forms the spec allows: a number of
// seconds, or an HTTP date. An unparseable value yields zero, which falls back
// to the client's own backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// drainClose empties and closes a body so its connection returns to the pool
// before the next attempt reuses it.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// parseError turns an error reply into an *APIError.
//
// It falls back rather than failing. A proxy or load balancer in front of the
// daemon can return an error page that is not our envelope at all, and a client
// that panics on a 502 from nginx is a client that breaks exactly when the
// service is already having a bad day.
func parseError(resp *http.Response) error {
	out := &APIError{
		StatusCode: resp.StatusCode,
		Type:       ErrorTypeApiError,
		Code:       "unknown",
		Message:    http.StatusText(resp.StatusCode),
		RequestID:  resp.Header.Get("X-Request-Id"),
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(raw) == 0 {
		return out
	}

	var env ErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
		// Not our envelope. Carry the body as the message so the caller can at
		// least see what answered them.
		out.Message = strings.TrimSpace(string(raw))
		return out
	}

	out.Type = env.Error.Type
	out.Code = env.Error.Code
	out.Message = env.Error.Message
	if env.Error.Param != nil {
		out.Param = *env.Error.Param
	}
	if env.Error.RequestId != nil {
		out.RequestID = *env.Error.RequestId
	}
	return out
}

// Health reports whether the daemon is up. It needs no token.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.do(ctx, http.MethodGet, "/health", nil, &out)
	return out, err
}

// Ptr returns a pointer to v.
//
// The generated params make every optional field a pointer, so that an absent
// field is distinguishable from a zero one -- which matters, because for some
// fields zero is a real, different choice. That is correct and unhelpful to
// type: microvm.Ptr(7) is what lets you write Priority: microvm.Ptr(7) without a
// throwaway variable for every optional you set.
func Ptr[T any](v T) *T { return &v }
