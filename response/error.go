package response

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

const (
	headerContentType  = "Content-Type"
	headerCacheControl = "Cache-Control"
	noStore            = "no-store"
	contentTypeJSON    = "application/json"
)

// Error is a failure that answers the client with the error envelope. Return
// it from a typed operation; the framework writes it.
type Error struct {
	// Status is the HTTP status of the answer.
	Status int
	// Code is the stable, camelCase code clients key on.
	Code string
	// Message is for whoever reads the body in a log or a trace.
	Message string
	// Details carries what the client needs to act on the failure; it is
	// written as JSON.
	Details any
	// Err is the failure behind a 5xx: the framework logs it, the client
	// never sees it.
	Err error
}

// Error describes the failure, its cause included.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// NewError returns the failure answering status with the envelope of code,
// message and details.
func NewError(status int, code, message string, details any) *Error {
	return &Error{Status: status, Code: code, Message: message, Details: details}
}

// BadRequest is a 400.
func BadRequest(code, message string, details any) *Error {
	return NewError(http.StatusBadRequest, code, message, details)
}

// Unauthorized is a 401.
func Unauthorized(code, message string, details any) *Error {
	return NewError(http.StatusUnauthorized, code, message, details)
}

// PaymentRequired is a 402.
func PaymentRequired(code, message string, details any) *Error {
	return NewError(http.StatusPaymentRequired, code, message, details)
}

// Forbidden is a 403.
func Forbidden(code, message string, details any) *Error {
	return NewError(http.StatusForbidden, code, message, details)
}

// NotFound is a 404.
func NotFound(code, message string, details any) *Error {
	return NewError(http.StatusNotFound, code, message, details)
}

// Conflict is a 409.
func Conflict(code, message string, details any) *Error {
	return NewError(http.StatusConflict, code, message, details)
}

// UnprocessableEntity is a 422.
func UnprocessableEntity(code, message string, details any) *Error {
	return NewError(http.StatusUnprocessableEntity, code, message, details)
}

// TooManyRequests is a 429.
func TooManyRequests(code, message string, details any) *Error {
	return NewError(http.StatusTooManyRequests, code, message, details)
}

// InternalServerError is a 500 caused by err, which the framework reports
// once, at Error; the client gets the envelope, never err.
func InternalServerError(err error, code, message string, details any) *Error {
	e := NewError(http.StatusInternalServerError, code, message, details)
	e.Err = err
	return e
}

// ErrorBody is the error envelope, what the client reads.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   any    `json:"details,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Body returns the envelope of e, stamped now.
func (e *Error) Body() ErrorBody {
	return ErrorBody{
		Code:      strings.ToValidUTF8(e.Code, "�"),
		Message:   strings.ToValidUTF8(e.Message, "�"),
		Details:   e.Details,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// encode encodes the envelope. Details that cannot be encoded are dropped
// rather than losing the whole answer, and an envelope that still cannot be
// encoded falls back to a 500 written by hand.
func (e *Error) encode() (status int, body []byte) {
	envelope := e.Body()
	if encoded, err := json.Marshal(envelope, json.Deterministic(true)); err == nil {
		return e.Status, encoded
	}
	envelope.Details = nil
	if encoded, err := json.Marshal(envelope, json.Deterministic(true)); err == nil {
		return e.Status, encoded
	}
	return http.StatusInternalServerError, []byte(`{"code":"serverError","message":` +
		strconv.Quote("failed to encode the error envelope") + `,"timestamp":"` + envelope.Timestamp + `"}`)
}

// Write answers w with the envelope of e. An error is never cached.
func (e *Error) Write(w http.ResponseWriter) {
	status, body := e.encode()
	w.Header().Set(headerContentType, contentTypeJSON)
	w.Header().Set(headerCacheControl, noStore)
	w.WriteHeader(status)
	_, _ = w.Write(body) //nolint:errcheck // the client went away; there is nobody left to tell
}

// APIGatewayV2 returns the envelope of e as an API Gateway HTTP API response,
// for a handler that answers the event itself rather than through a router.
func (e *Error) APIGatewayV2() events.APIGatewayV2HTTPResponse {
	status, body := e.encode()
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Body:       string(body),
		Headers: map[string]string{
			headerContentType:  contentTypeJSON,
			headerCacheControl: noStore,
		},
	}
}
