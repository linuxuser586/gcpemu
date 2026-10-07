package sql

import (
	"fmt"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Errors in the shape sqladmin returns (FR-CORE-025): the legacy
// errors[].reason carries Cloud SQL's lowerCamel reason.

func errInstanceNotFound() error {
	return apierr.NotFound("The Cloud SQL instance does not exist.").WithLegacy("instanceDoesNotExist").
		WithReason(errDomain, "INSTANCE_DOES_NOT_EXIST")
}

func errInstanceExists() error {
	return apierr.AlreadyExists("The Cloud SQL instance already exists.").WithLegacy("instanceAlreadyExists").
		WithReason(errDomain, "INSTANCE_ALREADY_EXISTS")
}

func errInvalid(format string, args ...any) error {
	return apierr.InvalidArgument("Invalid request: "+format, args...).WithLegacy("invalid")
}

func errNotRunning() error {
	return apierr.InvalidArgument("Invalid request since instance is not running.").WithLegacy("invalid")
}

func errOpNotFound(name string) error {
	return apierr.NotFound("The Cloud SQL operation %s does not exist.", name).WithLegacy("operationDoesNotExist")
}

func errDatabaseNotFound(name string) error {
	return apierr.NotFound("Not Found: database %q does not exist.", name).WithLegacy("notFound")
}

func errUserNotFound(name string) error {
	return apierr.NotFound("Not Found: user %q does not exist.", name).WithLegacy("notFound")
}

func errInProgress() error {
	return apierr.Aborted("Operation failed because another operation was already in progress.").
		WithLegacy("operationInProgress").WithHTTP(409)
}

func errUnsupported(what string) error {
	return apierr.Unimplemented("%s is not supported by the emulator.", what).WithLegacy("notImplemented")
}

// opError is an error recorded on an Operation (sql#operationError).
type opError struct {
	Code    string
	Message string
}

func (e *opError) Error() string { return e.Code + ": " + e.Message }

func newOpError(code, format string, args ...any) *opError {
	return &opError{Code: code, Message: fmt.Sprintf(format, args...)}
}
