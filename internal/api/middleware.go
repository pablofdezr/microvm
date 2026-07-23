package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/auth"
	"github.com/pablofdezr/microvm/internal/id"
)

// requestIDPrefix marks an ID as belonging to a request rather than an object.
const requestIDPrefix = "req"

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxToken
	ctxPrincipal
)

// requestIDFrom returns the request's ID, or "" outside a request.
func requestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// tokenFrom returns the bearer token the caller authenticated with.
//
// It scopes idempotency keys: two tenants who both pick the key "1" must not
// see each other's replies, and nothing stops them picking the same key.
func tokenFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxToken).(string)
	return v
}

// principalFrom returns who the caller is, or nil when auth is disabled.
//
// Nil is a real answer, not a missing one: a daemon with no tokens has no
// identities to resolve, and the storage layer reads a nil principal as "fall
// back to a per-sandbox namespace" rather than as an error.
func principalFrom(ctx context.Context) *auth.Principal {
	v, _ := ctx.Value(ctxPrincipal).(*auth.Principal)
	return v
}

// withRequestID gives every request an ID and echoes it back.
//
// It is the first thing wrapped and the last thing to fail, because everything
// else -- the log line, the error envelope, the caller's bug report -- refers to
// it. An error a caller can quote is an error we can find.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := id.New(requestIDPrefix)
		w.Header().Set("X-Request-Id", reqID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, reqID)))
	})
}

// auth checks the bearer token and attaches who it belongs to.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dir == nil {
			// Authentication disabled. There is no identity to attach, so the
			// principal stays nil and storage falls back to per-sandbox scoping.
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get("Authorization")
		if header == "" {
			s.writeAPIError(w, r, unauthorizedError(CodeTokenMissing,
				"No token provided. Send it as: Authorization: Bearer <token>."))
			return
		}

		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			s.writeAPIError(w, r, unauthorizedError(CodeTokenMissing,
				"Malformed Authorization header. Expected: Authorization: Bearer <token>."))
			return
		}
		principal, ok := s.dir.Resolve(token)
		if !ok {
			s.writeAPIError(w, r, unauthorizedError(CodeTokenInvalid, "Invalid token."))
			return
		}

		ctx := context.WithValue(r.Context(), ctxToken, token)
		ctx = context.WithValue(ctx, ctxPrincipal, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAdmin gates a route behind an admin token. It runs after auth, so a
// principal is already resolved by the time it looks.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth disabled means a dev daemon where every caller is already
		// everything; gating admin here would be theatre, since there is nobody
		// to be non-admin.
		if s.dir == nil {
			next.ServeHTTP(w, r)
			return
		}
		if p := principalFrom(r.Context()); p == nil || !p.Admin {
			s.writeAPIError(w, r, forbiddenError("This endpoint requires an admin token."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withRecovery turns a panic into a 500 rather than taking the daemon down.
// One malformed request must not kill every sandbox on the node.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					"request_id", requestIDFrom(r.Context()),
					"method", r.Method, "path", r.URL.Path, "panic", rec)
				s.writeAPIError(w, r, internalError(errors.New("panic")))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		// Health is polled constantly; logging it drowns everything else.
		if strings.HasSuffix(r.URL.Path, "/health") {
			return
		}
		s.log.Info("request",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration", time.Since(start).Round(time.Millisecond))
	})
}

// statusWriter records the status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.NewResponseController reach the real writer, which the
// streaming handler needs in order to flush.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// --- idempotency ------------------------------------------------------------

// idempotencyTTL is how long a key's reply is replayable. A day is long enough
// to cover any retry a sane client makes and short enough that the memory is
// bounded by a day's traffic rather than by uptime.
const idempotencyTTL = 24 * time.Hour

// maxIdempotencyEntries bounds the store regardless of traffic. Keys come from
// callers, so an unbounded map is a memory exhaustion primitive handed to
// anyone with a token.
const maxIdempotencyEntries = 10_000

// idempotencyEntry is one key's outcome.
type idempotencyEntry struct {
	// requestHash detects a key reused with a different request. Without it,
	// "retry" and "different call that happens to reuse a key" are the same
	// event, and the second silently gets the first one's answer.
	requestHash string
	createdAt   time.Time

	// done is closed once the first request finishes. A second request holding
	// the same key waits on nothing -- it is rejected -- but the channel is what
	// distinguishes in-flight from finished.
	done chan struct{}

	status int
	body   []byte
}

// idempotencyStore replays the replies of requests that already happened.
type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]*idempotencyEntry
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{entries: make(map[string]*idempotencyEntry)}
}

// withIdempotency makes an unsafe method safe to retry.
//
// The problem it solves is not hypothetical and has no client-side fix: a
// request whose reply is lost -- a timeout, a dropped connection, a proxy
// giving up -- cannot be known to have happened. The caller's only options are
// to retry (and maybe run the work twice) or not to (and maybe never run it at
// all). A key turns that into a third option: retry, and get the original
// answer.
//
// Only applied to the endpoints that create things. A GET is already idempotent
// and a stream cannot be replayed from a buffer.
func (s *Server) withIdempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		body, err := readBody(w, r)
		if err != nil {
			s.writeAPIError(w, r, err)
			return
		}

		// Scoped to the token: keys are caller-chosen, so two tenants picking
		// "1" is not a coincidence to plan around, it is the default.
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		storeKey := tokenFrom(r.Context()) + "|" + r.Method + "|" + r.URL.Path + "|" + key

		entry, replay, apiErr := s.idem.claim(storeKey, hash)
		if apiErr != nil {
			s.writeAPIError(w, r, apiErr)
			return
		}
		if replay {
			// The same request, already answered. Give back the same answer.
			w.Header().Set("Idempotent-Replayed", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(entry.status)
			_, _ = w.Write(entry.body)
			return
		}

		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Only a success is worth replaying. Replaying a 500 would pin a
		// transient failure to the key forever, so the one retry that could have
		// worked never gets to run.
		s.idem.finish(storeKey, rec.status, rec.body.Bytes(), rec.status < 400)
	})
}

// claim reserves a key, or reports what to do instead.
//
// Returns (entry, true, nil) to replay, (nil, false, nil) to run the handler,
// and an error when the key cannot be honoured.
func (st *idempotencyStore) claim(key, hash string) (*idempotencyEntry, bool, *apiError) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if existing, ok := st.entries[key]; ok {
		if existing.requestHash != hash {
			return nil, false, idempotencyError(
				"This Idempotency-Key was already used with a different request body. " +
					"A key must identify one request; use a new key for a new request.")
		}
		select {
		case <-existing.done:
			return existing, true, nil
		default:
			// The first request is still running. Making the second wait would
			// tie two callers' fates together and turn a slow create into two
			// slow creates; telling them to retry is honest and cheap.
			return nil, false, conflictError(CodeIdempotencyKeyReused,
				"A request with this Idempotency-Key is still in flight. Retry once it completes.")
		}
	}

	st.evictLocked()
	st.entries[key] = &idempotencyEntry{
		requestHash: hash,
		createdAt:   time.Now(),
		done:        make(chan struct{}),
	}
	return nil, false, nil
}

// finish records a reply, or drops the claim so the key can be tried again.
func (st *idempotencyStore) finish(key string, status int, body []byte, keep bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	entry, ok := st.entries[key]
	if !ok {
		return
	}
	if !keep {
		// The work failed. Free the key rather than pinning the failure to it:
		// a caller retrying after a 500 wants another attempt, not a recording
		// of the last one.
		delete(st.entries, key)
		close(entry.done)
		return
	}
	entry.status = status
	entry.body = body
	close(entry.done)
}

// evictLocked drops expired entries, and the oldest if the store is still full.
func (st *idempotencyStore) evictLocked() {
	now := time.Now()
	for k, e := range st.entries {
		if now.Sub(e.createdAt) > idempotencyTTL {
			delete(st.entries, k)
		}
	}
	if len(st.entries) < maxIdempotencyEntries {
		return
	}

	// Still full: drop the oldest. Losing a key means a retry runs twice, which
	// is bad; running out of memory means every sandbox on the node dies, which
	// is worse.
	var oldestKey string
	var oldest time.Time
	for k, e := range st.entries {
		if oldestKey == "" || e.createdAt.Before(oldest) {
			oldestKey, oldest = k, e.createdAt
		}
	}
	delete(st.entries, oldestKey)
}

// recordingWriter captures a reply so it can be replayed.
type recordingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (w *recordingWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.body.Write(p)
	return w.ResponseWriter.Write(p)
}
