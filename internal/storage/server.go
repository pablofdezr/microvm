package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server answers one sandbox's storage calls.
//
// # Why there is no authentication here
//
// There is no token, no key, and no caller identity anywhere in this file, and
// that is not an omission. This server is reached over one Unix socket living
// inside one sandbox's jail, so every connection it will ever accept came from
// that sandbox: the identity is the socket, established by the host before the
// VM booted. There is nothing for the guest to present because there is nothing
// it could present that would be worth more than the fact of where it connected
// from -- and a token would be strictly worse, because a token is a thing that
// can be stolen and a path is not.
//
// The corollary is a hard rule: one Server per sandbox, one listener per Server.
// Serving two sandboxes from one listener would not fail, would not warn, and
// would hand every sandbox every other sandbox's data. If you ever find yourself
// adding a sandbox ID to a request here, something upstream has already broken.
type Server struct {
	store *Store
	log   *slog.Logger
}

// NewServer returns a server bound to one sandbox's store.
func NewServer(store *Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: store, log: log}
}

// Serve runs until the listener closes or ctx ends.
//
// The listener closing is the normal exit, not an error: it is what the runtime
// does when the sandbox stops, and it is how this goroutine learns the sandbox
// it was serving no longer exists.
func Serve(ctx context.Context, l net.Listener, store *Store, log *slog.Logger) error {
	srv := &http.Server{
		Handler: NewServer(store, log),

		// A hostile guest can open a connection and then say nothing, forever. On
		// a normal server that is a slowloris; here it is a goroutine and a file
		// descriptor per attempt, on the host, from code we already assume is
		// trying. The header timeout is the one that stops it.
		ReadHeaderTimeout: 10 * time.Second,

		// No WriteTimeout on purpose: a read of a large object legitimately takes
		// as long as the object is big, and a deadline here would sever it
		// mid-stream at a size that depends on the guest's bandwidth. The body
		// size is bounded by the quota instead, which is the honest bound.
	}

	// Closing the server on ctx is what makes cancellation reach an in-flight
	// request. Without it, Serve returns but the connections it spawned live on.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			srv.Close()
		case <-done:
		}
	}()

	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/objects/"), r.URL.Path == "/objects":
		s.objects(w, r, strings.TrimPrefix(r.URL.Path, "/objects"))
	case strings.HasPrefix(r.URL.Path, "/dir/"), r.URL.Path == "/dir":
		s.list(w, r, strings.TrimPrefix(r.URL.Path, "/dir"))
	case r.URL.Path == "/usage":
		s.usage(w, r)
	default:
		s.fail(w, r, fmt.Errorf("%w: no such endpoint %q", ErrNotFound, r.URL.Path))
	}
}

func (s *Server) objects(w http.ResponseWriter, r *http.Request, path string) {
	switch r.Method {
	case http.MethodGet:
		s.get(w, r, path)
	case http.MethodPut:
		s.put(w, r, path)
	case http.MethodHead:
		s.stat(w, r, path)
	case http.MethodDelete:
		s.remove(w, r, path)
	default:
		w.Header().Set("Allow", "GET, PUT, HEAD, DELETE")
		s.fail(w, r, fmt.Errorf("%w: %s is not allowed on an object", ErrUnsupported, r.Method))
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, path string) {
	offset, length, err := parseRange(r.Header.Get("Range"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	body, err := s.store.Open(r.Context(), path, offset, length)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	// The status is written before the copy, so a failure partway through cannot
	// be reported as a status -- the header is already gone. The guest sees a
	// short body, which for a filesystem read is exactly what a short read is.
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		s.log.Warn("storage read cut short", "path", path, "err", err)
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, path string) {
	// ContentLength is -1 when the guest streams without declaring a length,
	// which the store handles: an undeclared size reserves nothing and is bounded
	// by the counting reader instead. Passing it straight through keeps the "a
	// declared size is a claim, not a fact" rule in one place.
	if err := s.store.Create(r.Context(), path, r.Body, r.ContentLength); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) stat(w http.ResponseWriter, r *http.Request, path string) {
	info, err := s.store.Stat(r.Context(), path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeInfoHeaders(w, info)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, path string) {
	if err := s.store.Remove(r.Context(), path); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		s.fail(w, r, fmt.Errorf("%w: %s is not allowed on a directory", ErrUnsupported, r.Method))
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.store.List(r.Context(), path, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	// statfs asks this, and answering it honestly is what makes `df` inside the
	// guest tell the truth about a quota it cannot see any other way.
	writeJSON(w, http.StatusOK, s.store.Usage())
}

func writeInfoHeaders(w http.ResponseWriter, info ObjectInfo) {
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("X-Object-Key", info.Key)
	if !info.LastModified.IsZero() {
		w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	}
	if info.ETag != "" {
		w.Header().Set("ETag", `"`+info.ETag+`"`)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is what the guest's filesystem gets when something fails.
//
// Errno rather than only a message, because the consumer is a filesystem and
// its caller is a program calling open(). "quota exceeded" is unactionable
// prose; EDQUOT is a thing Python raises as an OSError with the right subclass.
// Deciding the errno here rather than in the FUSE daemon keeps the knowledge of
// what went wrong next to the code that knows it.
type errorBody struct {
	Error string `json:"error"`
	Errno string `json:"errno"`
}

// fail maps a store error onto a status and an errno.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, errno := http.StatusInternalServerError, "EIO"

	switch {
	case errors.Is(err, ErrNotFound):
		status, errno = http.StatusNotFound, "ENOENT"

	case errors.Is(err, ErrEscapesPrefix):
		// 404 and ENOENT, exactly as if the path did not exist -- which, from
		// inside the sandbox, is the truth. The guest learns nothing about what
		// is out there, not even that "out there" is a thing.
		//
		// But it is logged at a level a missing file is not: an escape attempt is
		// never a typo. A program probing ../ is a program that already decided to.
		status, errno = http.StatusNotFound, "ENOENT"
		s.log.Warn("sandbox tried to escape its storage prefix",
			"path", r.URL.Path, "err", err)

	case errors.Is(err, ErrQuotaExceeded):
		// 507 rather than 429: the guest is not being rate-limited into retrying
		// later, it is out of room. EDQUOT says the same to the program inside.
		status, errno = http.StatusInsufficientStorage, "EDQUOT"

	case errors.Is(err, ErrReadOnly):
		status, errno = http.StatusForbidden, "EROFS"

	case errors.Is(err, ErrUnsupported):
		// This is the honest refusal the package comment is about: rename and
		// append do not work, and saying so loudly beats emulating them badly.
		status, errno = http.StatusNotImplemented, "EOPNOTSUPP"

	default:
		s.log.Error("storage call failed", "path", r.URL.Path, "err", err)
	}

	writeJSON(w, status, errorBody{Error: err.Error(), Errno: errno})
}

// parseRange reads the subset of HTTP ranges a filesystem actually produces.
//
// Deliberately not the full grammar: multipart ranges and suffix ranges
// ("bytes=-500") are not things a FUSE read turns into, and supporting them
// would mean supporting a multipart response for the sake of a caller that
// cannot ask for one. An empty header means the whole object.
func parseRange(header string) (offset, length int64, err error) {
	if header == "" {
		return 0, -1, nil
	}

	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok {
		return 0, 0, fmt.Errorf("%w: range %q is not a byte range", ErrUnsupported, header)
	}
	if strings.Contains(spec, ",") {
		return 0, 0, fmt.Errorf("%w: multipart ranges (%q) are not supported", ErrUnsupported, header)
	}

	start, end, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%w: range %q has no dash", ErrUnsupported, header)
	}
	if start == "" {
		return 0, 0, fmt.Errorf("%w: suffix ranges (%q) are not supported", ErrUnsupported, header)
	}

	offset, err = strconv.ParseInt(start, 10, 64)
	if err != nil || offset < 0 {
		return 0, 0, fmt.Errorf("%w: range %q has a bad offset", ErrUnsupported, header)
	}

	if end == "" {
		return offset, -1, nil // "bytes=100-" is "from 100 to the end"
	}

	last, err := strconv.ParseInt(end, 10, 64)
	if err != nil || last < offset {
		return 0, 0, fmt.Errorf("%w: range %q ends before it starts", ErrUnsupported, header)
	}

	// Inclusive at both ends, so the length is one more than the difference. The
	// same off-by-one that byteRange guards on the way out, in reverse.
	return offset, last - offset + 1, nil
}
