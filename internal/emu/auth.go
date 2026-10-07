package emu

import (
	"context"
	"log/slog"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
)

type principalKey struct{}

// Principal identifies an API caller, e.g. "user:dev@example.com" or
// "serviceAccount:sa@p.iam.gserviceaccount.com".
type Principal string

// Email returns the principal without its type prefix.
func (p Principal) Email() string {
	_, e, ok := strings.Cut(string(p), ":")
	if !ok {
		return string(p)
	}
	return e
}

// WithPrincipal stores the caller in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller stored in ctx.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// Authenticator maps a bearer token to a principal (FR-CORE-051). It is
// provided by the IAM service.
type Authenticator interface {
	// Authenticate returns the principal for token, or ok=false if unknown.
	Authenticate(ctx context.Context, token string) (p Principal, ok bool)
}

// Authorizer checks permissions (FR-IAM-004, FR-INT-012).
type Authorizer interface {
	// Check returns a PERMISSION_DENIED error when the caller in ctx lacks
	// permission on resource (a full resource name such as
	// "//storage.googleapis.com/projects/_/buckets/b") and enforcement is on.
	Check(ctx context.Context, permission, resource string) error
	// Authenticate resolves a bearer token.
	Authenticator
	// Mode returns the enforcement mode.
	Mode() string
}

// PolicyEvaluator decides whether a principal holds a permission on a
// resource. The IAM service provides the real one.
type PolicyEvaluator interface {
	Allowed(ctx context.Context, p Principal, permission, resource string) bool
}

// PolicyAuthorizer implements Authorizer using pluggable authenticator and
// evaluator, applying the configured mode.
type PolicyAuthorizer struct {
	mode  string
	log   *slog.Logger
	authn Authenticator
	eval  PolicyEvaluator
}

// NewPolicyAuthorizer returns an authorizer that allows everything until an
// evaluator is installed.
func NewPolicyAuthorizer(mode string, log *slog.Logger) *PolicyAuthorizer {
	return &PolicyAuthorizer{mode: mode, log: log}
}

// Install sets the authenticator and evaluator (called by the IAM service).
func (a *PolicyAuthorizer) Install(authn Authenticator, eval PolicyEvaluator) {
	a.authn, a.eval = authn, eval
}

func (a *PolicyAuthorizer) Mode() string { return a.mode }

func (a *PolicyAuthorizer) Authenticate(ctx context.Context, token string) (Principal, bool) {
	if a.authn == nil {
		return "", false
	}
	return a.authn.Authenticate(ctx, token)
}

func (a *PolicyAuthorizer) Check(ctx context.Context, permission, resource string) error {
	if a.mode == config.IAMOff || a.eval == nil {
		return nil
	}
	p := PrincipalFrom(ctx)
	if a.eval.Allowed(ctx, p, permission, resource) {
		return nil
	}
	if a.mode == config.IAMAudit {
		a.log.Warn("iam audit: would deny", "principal", string(p), "permission", permission, "resource", resource)
		return nil
	}
	return apierr.PermissionDenied("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource).
		WithReason("iam.googleapis.com", "IAM_PERMISSION_DENIED")
}
