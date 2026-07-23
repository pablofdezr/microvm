package api

import (
	"errors"
	"fmt"
	"net/http"

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
