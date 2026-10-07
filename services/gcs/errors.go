package gcs

import (
	"net/http"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// GCS-specific errors with the messages and legacy reasons the JSON API
// returns (FR-CORE-025).

func errBucketNotFound() *apierr.Error {
	return apierr.NotFound("The specified bucket does not exist.")
}

func errObjectNotFound(bucket, name string) *apierr.Error {
	return apierr.NotFound("No such object: %s/%s", bucket, name)
}

func errPrecondition() *apierr.Error {
	return apierr.New(codes.FailedPrecondition, "At least one of the pre-conditions you specified did not hold.").
		WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed)
}

func errNotModified() *apierr.Error {
	return apierr.New(codes.FailedPrecondition, "Not Modified").WithLegacy("notModified").WithHTTP(http.StatusNotModified)
}

func errInvalid(format string, args ...any) *apierr.Error {
	return apierr.InvalidArgument(format, args...)
}

func errRequired(param string) *apierr.Error {
	return apierr.InvalidArgument("Required parameter: %s", param).WithLegacy("required")
}

func errForbidden(format string, args ...any) *apierr.Error {
	return apierr.PermissionDenied(format, args...)
}

func errConflict(format string, args ...any) *apierr.Error {
	return apierr.New(codes.Aborted, format, args...).WithLegacy("conflict")
}
