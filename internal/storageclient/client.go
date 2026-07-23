// Package storageclient speaks the host storage server's HTTP protocol from
// inside the guest, over the guest->host vsock.
//
// # Why this is its own package, and why it holds no AWS
//
// This code is compiled into the guest agent, which ships inside every VM. The
// host's storage package imports the AWS SDK -- a large dependency -- and the
// guest must never carry it: not for size, and not for principle, because the
// guest is the untrusted side and has no business linking S3 at all. So this
// package re-declares the handful of wire types it needs rather than importing
// internal/storage, and depends on nothing heavier than net/http.
//
// # What it talks to
//
// One host-side storage.Server, reached over one vsock port (protocol.StoragePort)
// that Firecracker bridges to a Unix socket inside this sandbox's jail. There is
// no authentication because there is nothing to authenticate: the socket is the
// identity, fixed by the host before the VM booted. See internal/storage and
// internal/vsock for why that is sound.
//
// The protocol is plain HTTP/1.1:
//
//	GET    /objects/<path>   (Range header for partial reads)
//	PUT    /objects/<path>   (Content-Length declares the size)
//	HEAD   /objects/<path>   (metadata in headers)
//	DELETE /objects/<path>
//	GET    /dir/<path>?limit=&cursor=
//	GET    /usage
//
// Non-2xx replies carry a JSON {error, errno} body, and the errno is the whole
// point: the caller here is a filesystem, and "EDQUOT" is something a program's
// open() can act on where "quota exceeded" is just prose. See Error.
package storageclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DialFunc opens one connection to the host storage server.
//
// It is injected rather than hard-wired so the guest can supply the real vsock
// dial while tests supply an in-process pipe. Every call returns a fresh
// connection; the http.Transport built from it handles pooling.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client is one guest's handle to its storage. It is safe for concurrent use.
type Client struct {
	http *http.Client
	base string
}

// New returns a client that reaches the host storage server through dial.
func New(dial DialFunc) *Client {
	tr := &http.Transport{
		// The address is ignored: there is exactly one destination, the vsock
		// socket, and dial already knows it. This mirrors vsock.Transport on the
		// host side, for the same reason -- the URL's host is a placeholder.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		// A filesystem read of a large object streams for as long as the object
		// is big; compression would buffer to fill a block and defeat that.
		DisableCompression: true,

		// A small pool: connections are a cheap local socket, but re-dialling for
		// every getattr in a directory walk is wasteful, so keep a few warm.
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{
		http: &http.Client{Transport: tr},
		// The host is a placeholder; DialContext ignores it. It only has to be a
		// valid URL host so net/http will build a request at all.
		base: "http://storage",
	}
}

// ObjectInfo is what is known about an object without reading it.
//
// The JSON tags match the host's storage.ObjectInfo, which is serialised with
// its Go field names (no tags there), so a listing decodes straight into this.
type ObjectInfo struct {
	Key          string    `json:"Key"`
	Size         int64     `json:"Size"`
	LastModified time.Time `json:"LastModified"`
	ETag         string    `json:"ETag"`
}

// Listing is one page of a directory.
type Listing struct {
	Objects        []ObjectInfo `json:"Objects"`
	CommonPrefixes []string     `json:"CommonPrefixes"`
	Cursor         string       `json:"Cursor"`
}

// Usage is what the sandbox has stored, as the host meters it.
type Usage struct {
	BytesWritten     int64 `json:"BytesWritten"`
	Objects          int   `json:"Objects"`
	WritesLastMinute int   `json:"WritesLastMinute"`
	// Limit is the byte ceiling the sandbox faces, or 0 for unlimited. statfs
	// reports it as the filesystem's total size.
	Limit int64 `json:"Limit"`
}

// Error is a failed storage call, carrying the errno the host chose.
//
// The errno is what makes this useful to a filesystem: the FUSE layer maps it
// straight onto the syscall.Errno it returns to the guest program, so an
// out-of-quota write surfaces to Python as OSError(EDQUOT) rather than a generic
// I/O failure. Status is kept for the cases where the body was missing or
// unparseable and the HTTP status is all we have.
type Error struct {
	Op     string // "get", "put", ...
	Key    string
	Status int
	Errno  string // "ENOENT", "EDQUOT", ...; empty if the body carried none
	Msg    string
}

func (e *Error) Error() string {
	if e.Errno != "" {
		return fmt.Sprintf("storage %s %q: %s (%s)", e.Op, e.Key, e.Msg, e.Errno)
	}
	return fmt.Sprintf("storage %s %q: http %d: %s", e.Op, e.Key, e.Status, e.Msg)
}

// ErrnoOf returns the errno string carried by err, or "" if it is not a storage
// Error. The FUSE adapter uses this to translate without importing the concrete
// type into its errno table.
func ErrnoOf(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Errno
	}
	return ""
}

// Stat returns an object's metadata. The path is what the guest calls the file,
// e.g. "/logs/run.txt"; the host resolves it beneath this sandbox's prefix.
func (c *Client) Stat(ctx context.Context, path string) (ObjectInfo, error) {
	req, err := c.request(ctx, http.MethodHead, "/objects", path, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ObjectInfo{}, &Error{Op: "stat", Key: path, Msg: err.Error()}
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, c.errorFrom("stat", path, resp)
	}

	// HEAD carries the metadata in headers, not a body, because there is no body
	// to carry it in. Size is authoritative for a read plan, so a missing or
	// malformed Content-Length is an error rather than a silent zero.
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return ObjectInfo{}, &Error{Op: "stat", Key: path, Status: resp.StatusCode,
			Msg: fmt.Sprintf("bad Content-Length %q", resp.Header.Get("Content-Length"))}
	}
	info := ObjectInfo{
		Key:  resp.Header.Get("X-Object-Key"),
		Size: size,
		ETag: trimETag(resp.Header.Get("ETag")),
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			info.LastModified = t
		}
	}
	if info.Key == "" {
		info.Key = path
	}
	return info, nil
}

// Get opens an object for reading. length -1 means "to the end"; a bounded
// length is what a filesystem read of part of a large object becomes, and it
// must not drag the whole object across the socket. The caller closes the
// returned reader.
func (c *Client) Get(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	req, err := c.request(ctx, http.MethodGet, "/objects", path, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 || length >= 0 {
		req.Header.Set("Range", byteRange(offset, length))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Op: "get", Key: path, Msg: err.Error()}
	}
	// 206 is what a satisfiable Range yields; 200 is a whole-object read. Both
	// are success. Anything else is drained and mapped to an errno.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer drain(resp)
		return nil, c.errorFrom("get", path, resp)
	}
	return resp.Body, nil
}

// Put stores an object, replacing any existing one. size must be the exact
// number of bytes r will yield: the host reserves against the quota on it, and
// a wrong size is either a rejected write or a truncated object. A negative size
// is refused rather than streamed, because object storage cannot take a write
// whose length it does not know up front.
func (c *Client) Put(ctx context.Context, path string, r io.Reader, size int64) error {
	if size < 0 {
		return &Error{Op: "put", Key: path, Errno: "EINVAL",
			Msg: "a storage write needs a known length"}
	}
	req, err := c.request(ctx, http.MethodPut, "/objects", path, r)
	if err != nil {
		return err
	}
	// ContentLength is the claim the host reserves against. Setting it here, and
	// only here, keeps the "a declared size is a claim, not a fact" rule where
	// the host can still verify it against the bytes that actually arrive.
	req.ContentLength = size
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Op: "put", Key: path, Msg: err.Error()}
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return c.errorFrom("put", path, resp)
	}
	return nil
}

// Delete removes an object. Removing what is not there is not an error, matching
// the host and unlink() both.
func (c *Client) Delete(ctx context.Context, path string) error {
	req, err := c.request(ctx, http.MethodDelete, "/objects", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Op: "delete", Key: path, Msg: err.Error()}
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return c.errorFrom("delete", path, resp)
	}
	return nil
}

// List returns one page of a directory. cursor continues a previous page; an
// empty cursor starts one. limit 0 lets the host choose the page size.
func (c *Client) List(ctx context.Context, dir, cursor string, limit int) (Listing, error) {
	req, err := c.request(ctx, http.MethodGet, "/dir", dir, nil)
	if err != nil {
		return Listing{}, err
	}
	q := req.URL.Query()
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	req.URL.RawQuery = q.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return Listing{}, &Error{Op: "list", Key: dir, Msg: err.Error()}
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return Listing{}, c.errorFrom("list", dir, resp)
	}
	var out Listing
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Listing{}, &Error{Op: "list", Key: dir, Status: resp.StatusCode,
			Msg: "decode listing: " + err.Error()}
	}
	return out, nil
}

// Usage reports the sandbox's consumption, which is what statfs answers with.
func (c *Client) Usage(ctx context.Context) (Usage, error) {
	req, err := c.request(ctx, http.MethodGet, "/usage", "", nil)
	if err != nil {
		return Usage{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Usage{}, &Error{Op: "usage", Msg: err.Error()}
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return Usage{}, c.errorFrom("usage", "", resp)
	}
	var out Usage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Usage{}, &Error{Op: "usage", Status: resp.StatusCode,
			Msg: "decode usage: " + err.Error()}
	}
	return out, nil
}

// request builds a request whose URL path is endpoint + guestPath, correctly
// escaped. guestPath is the name as the guest spells it, with a leading slash.
func (c *Client) request(ctx context.Context, method, endpoint, guestPath string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	// Setting Path (the decoded form) lets url.URL do the percent-encoding on the
	// way out, so a key with a space or a "#" in it survives. endpoint has no
	// slash of its own; guestPath brings the leading slash.
	u.Path = endpoint + guestPath
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// errorFrom turns a non-2xx response into an *Error, reading the errno the host
// put in the body. A body that is missing or unparseable is not fatal: the
// status still tells the caller something, and a generic EIO is a safe default
// for a filesystem.
func (c *Client) errorFrom(op, key string, resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
		Errno string `json:"errno"`
	}
	// Bounded read: a hostile *host* is not the threat model, but a truncated or
	// endless error body still should not hang the guest's open().
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)

	errno := body.Errno
	if errno == "" {
		// A HEAD reply carries no body at all -- net/http strips it -- so the
		// errno the host wrote is gone by the time it arrives here. The status is
		// still there, and the host chose it from the same switch as the errno, so
		// recovering the errno from the status loses nothing. This is also the
		// safety net for any reply whose body did not parse.
		errno = errnoForStatus(resp.StatusCode)
	}
	return &Error{
		Op:     op,
		Key:    key,
		Status: resp.StatusCode,
		Errno:  errno,
		Msg:    body.Error,
	}
}

// errnoForStatus mirrors the host's status<->errno mapping in storage.Server.fail,
// for the replies (HEAD, or an unparseable body) that do not carry the errno
// themselves. It must stay in step with that switch.
func errnoForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "ENOENT"
	case http.StatusInsufficientStorage:
		return "EDQUOT"
	case http.StatusForbidden:
		return "EROFS"
	case http.StatusNotImplemented:
		return "EOPNOTSUPP"
	default:
		return "EIO"
	}
}

// byteRange renders a Range header the host's parseRange accepts. length -1 is
// an open-ended "from offset to the end".
func byteRange(offset, length int64) string {
	if length < 0 {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	if length == 0 {
		// A zero-length read is a real thing a filesystem asks. There is no empty
		// range to express it, so ask for the single byte at offset and let the
		// caller take none of it; the alternative is a whole-object fetch.
		return fmt.Sprintf("bytes=%d-%d", offset, offset)
	}
	// Ranges are inclusive at both ends, so the last byte is offset+length-1.
	return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
}

// trimETag strips the quotes S3-style ETags are wrapped in.
func trimETag(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// drain reads and closes a response body so the connection can be reused. Not
// draining it forces the pool to open a new connection next time.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
