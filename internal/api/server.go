// Package api serves the public HTTP interface.
//
// It is an adapter and nothing more. Every handler here does the same four
// things: read a request, call the core, map the result to a wire type, write
// it. Rules about what a sandbox may do, when it dies, or what a task costs
// live in the core and are not repeated here -- a rule enforced in an adapter
// is a rule that only applies to callers who arrive through that adapter.
//
// The wire types are generated from api/openapi.yaml into ./apitypes. They are
// deliberately not the core's types: the shape a caller sees is a contract we
// cannot change, whereas an internal struct three layers down must stay free to
// be refactored. Generating both the server's view and the SDKs' view from one
// spec is what keeps that separation from becoming a drift.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/auth"
	"github.com/pablofdezr/microvm/internal/pool"
	"github.com/pablofdezr/microvm/internal/queue"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/tenant"
)

// Version is reported by the health endpoint.
const Version = "1.0.0"

// APIVersion is the path prefix every route sits under.
const APIVersion = "v1"

// maxBodyBytes caps a request body. Callers upload source files, not datasets,
// and an unbounded body is a trivial way to exhaust the daemon.
const maxBodyBytes = 32 << 20 // 32 MiB

// Config configures the server.
type Config struct {
	// Tokens are the bearer tokens accepted. Empty disables authentication,
	// which is only ever correct on a loopback-bound dev instance -- this API
	// creates VMs that run arbitrary code, so an open one is an open shell.
	//
	// Each token maps to its own derived tenant, so listing tokens is enough to
	// give every caller a private, persistent storage namespace. For named
	// tenants, shared tenants, per-key quotas or read-only keys, use Principals.
	Tokens []string

	// Principals maps a token to an explicit identity, and takes precedence over
	// Tokens when set. It is how an operator grants a read-only key or a custom
	// quota -- anything the flat list cannot say.
	Principals map[string]*auth.Principal

	// Images the node can run.
	Images []string

	// Tenants stores per-tenant storage policies. Nil disables the tenant API
	// and leaves every tenant unlimited, which is the behaviour before policies
	// existed.
	Tenants tenant.Store
}

// Server serves the public API.
type Server struct {
	cfg  Config
	mgr  *sandbox.Manager
	q    queue.Queue
	pool *pool.Pool
	log  *slog.Logger
	idem *idempotencyStore

	// dir resolves a token to who is behind it. Nil when authentication is
	// disabled, which is the one case where there is no identity to resolve --
	// and, not coincidentally, the one case where storage falls back to a
	// per-sandbox namespace rather than a per-tenant one.
	dir *auth.Directory
}

// NewServer returns a Server.
//
// q and p may be nil on a node that serves sandboxes but takes no queued work.
func NewServer(cfg Config, mgr *sandbox.Manager, q queue.Queue, p *pool.Pool, log *slog.Logger) *Server {
	var dir *auth.Directory
	switch {
	case len(cfg.Principals) > 0:
		dir = auth.NewDirectoryOf(cfg.Principals)
	case len(cfg.Tokens) > 0:
		dir = auth.NewDirectory(cfg.Tokens)
	default:
		log.Warn("no API tokens configured: every caller is authorised",
			"hint", "only safe when bound to loopback")
	}
	return &Server{cfg: cfg, mgr: mgr, q: q, pool: p, log: log, idem: newIdempotencyStore(), dir: dir}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	p := "/" + APIVersion

	// Health needs no credentials: load balancers and uptime checks want it, and
	// it reveals nothing a caller could not guess.
	mux.Handle("GET "+p+"/health", http.HandlerFunc(s.handleHealth))

	// Creating things is where retries are dangerous, so those are the routes
	// that take an Idempotency-Key. A GET is already repeatable and a stream
	// cannot be replayed out of a buffer.
	create := func(h http.HandlerFunc) http.Handler {
		return s.auth(s.withIdempotency(h))
	}
	read := func(h http.HandlerFunc) http.Handler {
		return s.auth(h)
	}
	admin := func(h http.HandlerFunc) http.Handler {
		return s.auth(s.requireAdmin(h))
	}

	mux.Handle("POST "+p+"/sandboxes", create(s.handleCreateSandbox))
	mux.Handle("GET "+p+"/sandboxes", read(s.handleListSandboxes))
	mux.Handle("GET "+p+"/sandboxes/{sandbox}", read(s.handleRetrieveSandbox))
	mux.Handle("DELETE "+p+"/sandboxes/{sandbox}", read(s.handleDeleteSandbox))

	mux.Handle("POST "+p+"/sandboxes/{sandbox}/executions", create(s.handleCreateExecution))
	mux.Handle("GET "+p+"/sandboxes/{sandbox}/executions", read(s.handleListExecutions))
	mux.Handle("GET "+p+"/sandboxes/{sandbox}/executions/{execution}", read(s.handleRetrieveExecution))
	mux.Handle("GET "+p+"/sandboxes/{sandbox}/executions/{execution}/stream", read(s.handleStreamExecution))
	mux.Handle("POST "+p+"/sandboxes/{sandbox}/executions/{execution}/cancel", read(s.handleCancelExecution))

	mux.Handle("POST "+p+"/sandboxes/{sandbox}/files", read(s.handleCreateFile))
	mux.Handle("GET "+p+"/sandboxes/{sandbox}/files", read(s.handleRetrieveFile))

	mux.Handle("POST "+p+"/tasks", create(s.handleCreateTask))
	mux.Handle("GET "+p+"/tasks/{task}", read(s.handleRetrieveTask))

	mux.Handle("GET "+p+"/queue", read(s.handleRetrieveQueue))
	mux.Handle("GET "+p+"/images", read(s.handleListImages))

	mux.Handle("GET "+p+"/tenants", admin(s.handleListTenants))
	mux.Handle("PUT "+p+"/tenants/{tenant}", admin(s.handleUpdateTenant))
	mux.Handle("GET "+p+"/tenants/{tenant}", admin(s.handleRetrieveTenant))

	// An unmatched route gets the error envelope like everything else. Go's
	// default 404 is a bare text body, which would be the one reply in the API a
	// client's error handling could not parse.
	mux.Handle("/", http.HandlerFunc(s.handleNotFound))

	return withRequestID(s.withRecovery(s.withLogging(mux)))
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeAPIError(w, r, &apiError{
		status:  http.StatusNotFound,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    CodeRouteNotFound,
		message: fmt.Sprintf("Unrecognized request URL: %s %s", r.Method, r.URL.Path),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, apitypes.Health{
		Object:  apitypes.HealthObjectHealth,
		Ok:      true,
		Version: Version,
	})
}

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	data := make([]apitypes.Image, 0, len(s.cfg.Images))
	for _, name := range s.cfg.Images {
		data = append(data, apitypes.Image{
			Object:   apitypes.ImageObjectImage,
			Id:       name,
			Language: languageOf(name),
		})
	}
	writeJSON(w, http.StatusOK, apitypes.ImageList{
		Object:  apitypes.ImageListObjectList,
		Url:     "/" + APIVersion + "/images",
		HasMore: false,
		Data:    data,
	})
}

// languageOf guesses the language from an image's name.
//
// The image name is whatever file the operator dropped in the image directory,
// so this is a hint for display and nothing more -- never a decision.
func languageOf(image string) string {
	for _, lang := range []string{"python", "node", "go", "rust"} {
		if len(image) >= len(lang) && image[:len(lang)] == lang {
			return lang
		}
	}
	return "unknown"
}

func (s *Server) handleRetrieveQueue(w http.ResponseWriter, r *http.Request) {
	if s.q == nil {
		s.writeAPIError(w, r, notFoundError(CodeRouteNotFound, "queue", "this node takes no queued work"))
		return
	}

	stats, err := s.q.Stats(r.Context())
	if err != nil {
		s.writeAPIError(w, r, queueUnreachableError(err))
		return
	}

	out := apitypes.Queue{
		Object:          apitypes.QueueObjectQueue,
		Pending:         stats.Pending,
		Leased:          stats.Leased,
		Done:            stats.Done,
		Failed:          stats.Failed,
		OldestPendingMs: int(stats.OldestPending.Milliseconds()),
	}
	// Slots and Busy are this node's alone. No node knows the fleet's capacity,
	// and none needs to -- that is exactly what lets one be added without
	// telling anything else it exists.
	if s.pool != nil {
		out.Slots = s.pool.Slots()
		out.Busy = s.pool.Busy()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// readBody reads a request body under the size cap.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, invalidBodyError(err)
	}
	// The idempotency middleware reads the body to hash it, so the handler
	// downstream must still be able to read it.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// decodeBody reads and parses a JSON request body.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	body, err := readBody(w, r)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return invalidBodyError(fmt.Errorf("the body is empty"))
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	// A misspelled field is an error rather than a shrug. A caller who writes
	// "timeout_second" and is silently given the default has a bug they cannot
	// see; one who is told has a typo they can fix.
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		return invalidBodyError(err)
	}
	return nil
}

// decodeOptionalBody parses a body that may legitimately be absent.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, into any) error {
	body, err := readBody(w, r)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return invalidBodyError(err)
	}
	return nil
}
