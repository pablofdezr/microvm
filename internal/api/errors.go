package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
)

// apiError is a failure with everything a caller needs to react to it.
//
// The four fields answer four different questions, and collapsing any of them
// into the others is what makes an API annoying to use:
//
//	status  — what HTTP says happened
//	type    — what class of thing went wrong, i.e. what to *do*
//	code    — exactly what went wrong, stable enough to switch on
//	message — what to show a human
//
// The type is what most clients branch on: a `capacity_error` is worth
// retrying, an `invalid_request_error` never is, and no amount of parsing the
// message reveals which one you have.
type apiError struct {
	status  int
	errType apitypes.ErrorType
	code    string
	message string
	// param names the request field at fault, when one is.
	param string
	// retryAfter becomes the Retry-After header, when we know how long. Only the
	// rate limiter does: a full node has no idea when a slot frees, and a header
	// guessing at it would send every refused caller back at the same moment.
	retryAfter time.Duration
	// cause is kept for the log and never sent: it is our internals, and a
	// caller who can read them is a caller who can probe them.
	cause error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

func (e *apiError) Unwrap() error { return e.cause }

// Error codes. These are contract: a caller switches on them, so renaming one
// breaks code we cannot see.
const (
	CodeSandboxNotFound   = "sandbox_not_found"
	CodeExecutionNotFound = "execution_not_found"
	CodeTaskNotFound      = "task_not_found"
	CodeFileNotFound      = "file_not_found"
	CodeRouteNotFound     = "route_not_found"

	CodeParameterMissing = "parameter_missing"
	CodeParameterInvalid = "parameter_invalid"
	CodeBodyInvalid      = "body_invalid"

	CodeSandboxNotRunning = "sandbox_not_running"
	CodeAlreadyExists     = "resource_already_exists"

	CodeIdempotencyKeyReused = "idempotency_key_reused"

	CodeNodeAtCapacity   = "node_at_capacity"
	CodeQueueUnreachable = "queue_unreachable"

	CodeSourceNotPermitted = "source_not_permitted"
	CodeSourceFetchFailed  = "source_fetch_failed"

	CodeTokenMissing = "token_missing"
	CodeTokenInvalid = "token_invalid"
	CodeForbidden    = "forbidden"

	CodeTenantNotFound = "tenant_not_found"

	CodeInternalError = "internal_error"
)

// notFoundError reports that an object does not exist.
//
// It deliberately quotes the ID back. A 404 that does not say what it could not
// find is the least useful reply an API can send: the caller is left unable to
// tell a typo from a deleted object from a bug in their own ID handling.
func notFoundError(code, resource, id string) *apiError {
	return &apiError{
		status:  http.StatusNotFound,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    code,
		message: fmt.Sprintf("No such %s: %s", resource, id),
	}
}

func missingParamError(param string) *apiError {
	return &apiError{
		status:  http.StatusBadRequest,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    CodeParameterMissing,
		message: fmt.Sprintf("Missing required parameter: %s.", param),
		param:   param,
	}
}

func invalidParamError(param, why string) *apiError {
	return &apiError{
		status:  http.StatusBadRequest,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    CodeParameterInvalid,
		message: fmt.Sprintf("Invalid value for %s: %s", param, why),
		param:   param,
	}
}

func invalidBodyError(cause error) *apiError {
	return &apiError{
		status:  http.StatusBadRequest,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    CodeBodyInvalid,
		message: fmt.Sprintf("Could not parse the request body: %v", cause),
		cause:   cause,
	}
}

func unauthorizedError(code, message string) *apiError {
	return &apiError{
		status:  http.StatusUnauthorized,
		errType: apitypes.ErrorTypeAuthenticationError,
		code:    code,
		message: message,
	}
}

// forbiddenError reports a valid token that lacks the power for this. It is a
// 403, not a 401: the caller is authenticated and still not allowed, and telling
// them to re-authenticate would send them in a circle.
func forbiddenError(message string) *apiError {
	return &apiError{
		status:  http.StatusForbidden,
		errType: apitypes.ErrorTypeAuthenticationError,
		code:    CodeForbidden,
		message: message,
	}
}

func conflictError(code, message string) *apiError {
	return &apiError{
		status:  http.StatusConflict,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    code,
		message: message,
	}
}

// capacityError means no room, right now.
//
// 429 rather than 503: it is the caller's request that cannot be served, not
// the service that is down, and every client library already knows to back off
// on a 429. The message names the alternative, because there is one and a
// caller stuck in a retry loop may not know it.
func capacityError(cause error) *apiError {
	return &apiError{
		status:  http.StatusTooManyRequests,
		errType: apitypes.ErrorTypeCapacityError,
		code:    CodeNodeAtCapacity,
		message: "This node has no free capacity. Retry shortly, or submit a task instead: " +
			"tasks wait for a slot anywhere in the fleet rather than failing.",
		cause: cause,
	}
}

// rateLimitError means the caller is asking faster than their key allows.
//
// The same 429 and the same capacity_error as a full node, deliberately. That
// pair is the API's existing "no room right now, back off" and it is what all
// three SDKs switch on to retry; a new type or code would mean a wire vocabulary
// the SDKs do not know, for a condition they already handle correctly. What is
// new is Retry-After, and only here: this is the one refusal whose end we can
// name to the second.
func rateLimitError(rps float64, retryAfter time.Duration) *apiError {
	return &apiError{
		status:  http.StatusTooManyRequests,
		errType: apitypes.ErrorTypeCapacityError,
		code:    CodeNodeAtCapacity,
		message: fmt.Sprintf(
			"Too many requests: this token may make %s per second. Retry in %s.",
			plural(rps, "request"), plural(retryAfter.Seconds(), "second")),
		retryAfter: retryAfter,
	}
}

// tenantConcurrencyError means the caller's own sandboxes are the limit, not the
// node's.
//
// Reported as capacity, like a full node, because the caller's move is the same
// -- wait, then retry -- and because that is the type their SDK already backs
// off on. The message is the part that differs, and it has to: a caller told
// "this node is full" would keep retrying against a node with plenty of room,
// when what they need is to delete a sandbox of their own.
//
// No Retry-After. When one of their sandboxes ends is up to them, and a number
// invented here would be a guess dressed as a fact.
//
// The count includes a create still in progress, which is said out loud because
// otherwise the number contradicts every other endpoint: a create is charged its
// slot before it boots -- and, with -source-fetch, before its source is even
// downloaded -- so a caller can be told they hold one while GET /v1/sandboxes
// returns an empty list and there is nothing for them to delete.
func tenantConcurrencyError(cause error, live, limit int) *apiError {
	return &apiError{
		status:  http.StatusTooManyRequests,
		errType: apitypes.ErrorTypeCapacityError,
		code:    CodeNodeAtCapacity,
		message: fmt.Sprintf(
			"This token may have %d sandboxes running at once and already has %d, counting "+
				"any create still in progress. Delete one, or wait for one to expire.", limit, live),
		cause: cause,
	}
}

// plural renders a count with its unit, for a message a human reads.
func plural(n float64, unit string) string {
	// 'g' with -1 precision so 10 is "10" and 0.5 is "0.5": a limit printed as
	// "10.000000 requests" reads like a bug in the daemon.
	s := strconv.FormatFloat(n, 'g', -1, 64) + " " + unit
	if math.Abs(n-1) > 1e-9 {
		s += "s"
	}
	return s
}

// sourceNotPermittedError is a seed refused before a byte left this host.
//
// One code and one message for every reason: fetching is not enabled, the host is
// not on the operator's allowlist, the scheme is not https, the credential_ref
// names nothing, or the name resolves somewhere the daemon may not go. Told apart,
// they would be a port scanner with the host's routing table -- the daemon runs
// outside the firewall it installs for guests, so "that host does not resolve" and
// "that address is private" are facts about the operator's network. The detail is
// carried for the log, where the operator who set the policy is the one reading.
//
// A 400, because retrying it unchanged fails identically: the policy is not going
// to move in the next minute.
func sourceNotPermittedError(cause error) *apiError {
	return &apiError{
		status:  http.StatusBadRequest,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    CodeSourceNotPermitted,
		message: "This source cannot be fetched. The operator names the hosts a sandbox may be " +
			"seeded from, https is the only scheme, and a credential must be one the operator configured.",
		// The object rather than a field inside it: naming source.url would be wrong
		// half the time -- an unconfigured credential_ref is the same refusal -- and
		// which of them it was is the answer this error exists not to give.
		param: "source",
		cause: cause,
	}
}

// sourceFetchFailedError is a seed that was reached and did not survive.
//
// 502 and api_error rather than a 400: the URL is not necessarily wrong, the
// origin may answer in a minute, and "retry, possibly with the same
// Idempotency-Key" is exactly what api_error already tells a client to do.
//
// The detail is quoted, unlike the refusal above. Which hosts are reachable is the
// operator's own choice, so an HTTP status or a rejected archive member gives away
// nothing about the network that the allowlist did not already publish. The stage
// is named because it says whose problem it is: a fetch is the origin, an expand is
// the archive.
func sourceFetchFailedError(stage string, cause error) *apiError {
	return &apiError{
		status:  http.StatusBadGateway,
		errType: apitypes.ErrorTypeApiError,
		code:    CodeSourceFetchFailed,
		message: fmt.Sprintf("This sandbox's source failed at the %s stage: %v.", stage, cause),
		cause:   cause,
	}
}

// queueUnreachableError means we could not ask the queue.
//
// 503, not 404. "It does not exist" and "we cannot find out" demand opposite
// reactions -- the first says stop, the second says try again -- and a client
// told the wrong one gives up on work that is running perfectly well.
func queueUnreachableError(cause error) *apiError {
	return &apiError{
		status:  http.StatusServiceUnavailable,
		errType: apitypes.ErrorTypeApiError,
		code:    CodeQueueUnreachable,
		message: "The queue could not be reached, so nothing is known about this task right now. Retry.",
		cause:   cause,
	}
}

func idempotencyError(message string) *apiError {
	return &apiError{
		status:  http.StatusConflict,
		errType: apitypes.ErrorTypeIdempotencyError,
		code:    CodeIdempotencyKeyReused,
		message: message,
	}
}

// internalError is our fault.
//
// The cause is carried for the log but never for the wire: an internal error
// message is a description of our internals, and the caller can do nothing with
// it except learn things about us they should not know.
func internalError(cause error) *apiError {
	return &apiError{
		status:  http.StatusInternalServerError,
		errType: apitypes.ErrorTypeApiError,
		code:    CodeInternalError,
		message: "Something went wrong on our end. If it persists, quote the request id.",
		cause:   cause,
	}
}

// sandboxNotRunningError explains why work was refused.
//
// The reason is the useful part: "expired" and "failed" both stop a caller's
// work, but only one of them means something broke.
func sandboxNotRunningError(id, state, reason string) *apiError {
	msg := fmt.Sprintf("Sandbox %s is %s", id, state)
	if reason != "" {
		msg += fmt.Sprintf(" (%s)", reason)
	}
	return &apiError{
		status:  http.StatusConflict,
		errType: apitypes.ErrorTypeInvalidRequestError,
		code:    CodeSandboxNotRunning,
		message: msg + ".",
	}
}

// writeAPIError sends err as the error envelope.
//
// Anything that is not an *apiError is treated as an internal error, on the
// principle that an error nobody classified is an error nobody meant to show a
// caller.
func (s *Server) writeAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		apiErr = internalError(err)
	}

	reqID := requestIDFrom(r.Context())

	// Log our own failures with the cause; a caller's mistake is not an event
	// worth a line, or a scanner probing routes would fill the log.
	if apiErr.status >= 500 {
		s.log.Error("request failed",
			"request_id", reqID,
			"method", r.Method, "path", r.URL.Path,
			"code", apiErr.code, "err", apiErr.Error())
	}

	// Before the body, because writeJSON writes the status and headers are gone
	// after that. Seconds rather than a date: a clock skewed by a minute turns an
	// HTTP-date into either an instant retry or a minute of silence, and all three
	// SDKs already honour the numeric form.
	if apiErr.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(apiErr.retryAfter.Seconds()))))
	}

	body := apitypes.ErrorEnvelope{
		Error: apitypes.Error{
			Type:    apiErr.errType,
			Code:    apiErr.code,
			Message: apiErr.message,
		},
	}
	if apiErr.param != "" {
		body.Error.Param = &apiErr.param
	}
	if reqID != "" {
		body.Error.RequestId = &reqID
	}

	writeJSON(w, apiErr.status, body)
}
