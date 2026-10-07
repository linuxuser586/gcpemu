// Package apierr models GCP API errors and renders them as the JSON error
// envelope used by REST APIs and as google.rpc.Status for gRPC (FR-CORE-025).
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error is a GCP-shaped API error.
type Error struct {
	Code    codes.Code
	Message string
	// Reason is the ErrorInfo reason (UPPER_SNAKE), e.g. "RESOURCE_NOT_FOUND".
	Reason string
	// Domain is the ErrorInfo domain, e.g. "storage.googleapis.com".
	Domain string
	// LegacyReason is the v1 "errors[].reason" value (lowerCamel), e.g. "notFound".
	LegacyReason string
	// HTTPStatus overrides the status derived from Code (e.g. 412 for GCS preconditions).
	HTTPStatus int
	Metadata   map[string]string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// HTTP returns the HTTP status for the error.
func (e *Error) HTTP() int {
	if e.HTTPStatus != 0 {
		return e.HTTPStatus
	}
	return HTTPFromCode(e.Code)
}

// GRPCStatus implements the interface used by grpc/status.FromError.
func (e *Error) GRPCStatus() *status.Status {
	st := status.New(e.Code, e.Message)
	if e.Reason != "" {
		if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: e.Reason, Domain: e.Domain, Metadata: e.Metadata}); err == nil {
			return d
		}
	}
	return st
}

// New creates an error with a formatted message.
func New(code codes.Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithReason sets the ErrorInfo reason/domain and returns e.
func (e *Error) WithReason(domain, reason string) *Error {
	e.Domain, e.Reason = domain, reason
	return e
}

// WithLegacy sets the legacy errors[].reason and returns e.
func (e *Error) WithLegacy(reason string) *Error {
	e.LegacyReason = reason
	return e
}

// WithHTTP overrides the HTTP status and returns e.
func (e *Error) WithHTTP(code int) *Error {
	e.HTTPStatus = code
	return e
}

func NotFound(format string, args ...any) *Error {
	return New(codes.NotFound, format, args...).WithLegacy("notFound")
}

func AlreadyExists(format string, args ...any) *Error {
	return New(codes.AlreadyExists, format, args...).WithLegacy("conflict")
}

func InvalidArgument(format string, args ...any) *Error {
	return New(codes.InvalidArgument, format, args...).WithLegacy("invalid")
}

func FailedPrecondition(format string, args ...any) *Error {
	return New(codes.FailedPrecondition, format, args...).WithLegacy("failedPrecondition")
}

func PermissionDenied(format string, args ...any) *Error {
	return New(codes.PermissionDenied, format, args...).WithLegacy("forbidden")
}

func Unauthenticated(format string, args ...any) *Error {
	return New(codes.Unauthenticated, format, args...).WithLegacy("required")
}

func Unimplemented(format string, args ...any) *Error {
	return New(codes.Unimplemented, format, args...).WithLegacy("notImplemented")
}

func Internal(format string, args ...any) *Error {
	return New(codes.Internal, format, args...).WithLegacy("backendError")
}

func Aborted(format string, args ...any) *Error {
	return New(codes.Aborted, format, args...).WithLegacy("aborted")
}

// From converts any error into an *Error.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return &Error{Code: st.Code(), Message: st.Message()}
	}
	return Internal("%v", err)
}

// HTTPFromCode maps a canonical code to an HTTP status per google.rpc.Code docs.
func HTTPFromCode(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499
	case codes.InvalidArgument, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// CodeName returns the google.rpc.Code enum name (e.g. NOT_FOUND).
func CodeName(c codes.Code) string {
	switch c {
	case codes.OK:
		return "OK"
	case codes.Canceled:
		return "CANCELLED"
	case codes.Unknown:
		return "UNKNOWN"
	case codes.InvalidArgument:
		return "INVALID_ARGUMENT"
	case codes.DeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case codes.NotFound:
		return "NOT_FOUND"
	case codes.AlreadyExists:
		return "ALREADY_EXISTS"
	case codes.PermissionDenied:
		return "PERMISSION_DENIED"
	case codes.ResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case codes.FailedPrecondition:
		return "FAILED_PRECONDITION"
	case codes.Aborted:
		return "ABORTED"
	case codes.OutOfRange:
		return "OUT_OF_RANGE"
	case codes.Unimplemented:
		return "UNIMPLEMENTED"
	case codes.Internal:
		return "INTERNAL"
	case codes.Unavailable:
		return "UNAVAILABLE"
	case codes.DataLoss:
		return "DATA_LOSS"
	case codes.Unauthenticated:
		return "UNAUTHENTICATED"
	}
	return "UNKNOWN"
}

type legacyItem struct {
	Message string `json:"message"`
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
}

type detail struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason,omitempty"`
	Domain   string            `json:"domain,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type body struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Errors  []legacyItem `json:"errors,omitempty"`
	Status  string       `json:"status"`
	Details []detail     `json:"details,omitempty"`
}

// JSON renders the REST error envelope.
func (e *Error) JSON() []byte {
	b := body{Code: e.HTTP(), Message: e.Message, Status: CodeName(e.Code)}
	legacy := e.LegacyReason
	if legacy == "" {
		legacy = "backendError"
	}
	b.Errors = []legacyItem{{Message: e.Message, Domain: "global", Reason: legacy}}
	if e.Reason != "" {
		b.Details = []detail{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: e.Reason, Domain: e.Domain, Metadata: e.Metadata}}
	}
	out, _ := json.Marshal(map[string]any{"error": b})
	return out
}

// Write writes err as a REST JSON error response.
func Write(w http.ResponseWriter, err error) {
	e := From(err)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTP())
	_, _ = w.Write(e.JSON())
}
