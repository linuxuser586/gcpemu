package iam

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	credentialspb "cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	iamv1 "google.golang.org/api/iam/v1"
	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// IAM Credentials API (FR-IAM-007), REST and gRPC.

const maxTokenLifetime = 12 * time.Hour

// impersonate resolves the target account of name
// ("projects/-/serviceAccounts/X") and checks the delegation chain: the
// caller needs iam.serviceAccounts.implicitDelegation on delegates[0], each
// delegate on the next, and the last link perm on the target. Checks go
// through the authorizer, so the IAM mode applies.
func (s *Service) impersonate(ctx context.Context, name string, delegates []string, perm string) (*iamv1.ServiceAccount, error) {
	id, err := accountFromName(name)
	if err != nil {
		return nil, err
	}
	var target *iamv1.ServiceAccount
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { target, ok = s.getAccount(tx, id); return nil })
	targetRes := "//iam.googleapis.com/projects/-/serviceAccounts/" + id
	if ok {
		targetRes = saResource(target)
	} else if p := projectOfEmail(id); p != "" {
		targetRes = "//iam.googleapis.com/projects/" + p + "/serviceAccounts/" + id
	}
	for i, d := range delegates {
		did, err := accountFromName(d)
		if err != nil {
			return nil, err
		}
		var del *iamv1.ServiceAccount
		var dok bool
		_ = s.env.Store.View(func(tx store.Tx) error { del, dok = s.getAccount(tx, did); return nil })
		if !dok {
			return nil, apierr.NotFound("Delegate %s not found.", d).WithReason(iamDomain, "SERVICE_ACCOUNT_NOT_FOUND")
		}
		pctx := ctx
		if i > 0 {
			prev, _ := accountFromName(delegates[i-1])
			pctx = emu.WithPrincipal(ctx, emu.Principal("serviceAccount:"+prev))
		}
		if err := s.check(pctx, "iam.serviceAccounts.implicitDelegation", saResource(del)); err != nil {
			return nil, err
		}
	}
	last := ctx
	if len(delegates) > 0 {
		prev, _ := accountFromName(delegates[len(delegates)-1])
		last = emu.WithPrincipal(ctx, emu.Principal("serviceAccount:"+prev))
	}
	if err := s.check(last, perm, targetRes); err != nil {
		return nil, err
	}
	if !ok {
		return nil, apierr.NotFound("Requested entity was not found.").WithReason(iamDomain, "SERVICE_ACCOUNT_NOT_FOUND")
	}
	if target.Disabled {
		return nil, apierr.FailedPrecondition("Service account %s is disabled.", target.Email).WithReason(iamDomain, "SERVICE_ACCOUNT_DISABLED")
	}
	return target, nil
}

// generateAccessToken implements the shared logic of both transports.
func (s *Service) generateAccessToken(ctx context.Context, name string, delegates, scopes []string, lifetime time.Duration) (string, time.Time, error) {
	if len(scopes) == 0 {
		return "", time.Time{}, apierr.InvalidArgument("Scope required.")
	}
	if lifetime == 0 {
		lifetime = tokenLifetime
	}
	if lifetime < 0 || lifetime > maxTokenLifetime {
		return "", time.Time{}, apierr.InvalidArgument("The lifetime must be between 1 second and 43200 seconds.")
	}
	sa, err := s.impersonate(ctx, name, delegates, "iam.serviceAccounts.getAccessToken")
	if err != nil {
		return "", time.Time{}, err
	}
	return s.mintAccessToken(emu.Principal("serviceAccount:"+sa.Email), scopes, "", lifetime)
}

func (s *Service) generateIDToken(ctx context.Context, name string, delegates []string, audience string, includeEmail bool) (string, error) {
	if audience == "" {
		return "", apierr.InvalidArgument("Audience is required.")
	}
	sa, err := s.impersonate(ctx, name, delegates, "iam.serviceAccounts.getOpenIdToken")
	if err != nil {
		return "", err
	}
	return s.mintIDToken(sa.Email, audience, includeEmail, nil)
}

func (s *Service) signBlobFor(ctx context.Context, name string, delegates []string, payload []byte) (string, []byte, error) {
	sa, err := s.impersonate(ctx, name, delegates, "iam.serviceAccounts.signBlob")
	if err != nil {
		return "", nil, err
	}
	return s.signWithSystemKey(sa, payload)
}

func (s *Service) signJwtFor(ctx context.Context, name string, delegates []string, payload string) (string, string, error) {
	sa, err := s.impersonate(ctx, name, delegates, "iam.serviceAccounts.signJwt")
	if err != nil {
		return "", "", err
	}
	kid, jwt, err := s.signJWTWithSystemKey(sa, payload)
	if err != nil {
		return "", "", apierr.InvalidArgument("Invalid JWT payload: %v", err)
	}
	return kid, jwt, nil
}

// serveCredentials serves iamcredentials.googleapis.com v1 REST.
func (s *Service) serveCredentials(w http.ResponseWriter, r *http.Request) {
	rt := parseRoute(r.URL.Path)
	c, ok := rt.match(rt.verb, "v1", "projects", "*", "serviceAccounts", "*")
	if !ok || r.Method != http.MethodPost {
		notFoundRoute(w, r)
		return
	}
	name := "projects/" + c[0] + "/serviceAccounts/" + c[1]
	ctx := r.Context()
	var out any
	var err error
	switch rt.verb {
	case "generateAccessToken":
		var req iamcredentials.GenerateAccessTokenRequest
		if err = readJSON(r, &req); err != nil {
			break
		}
		var life time.Duration
		if req.Lifetime != "" {
			if life, err = time.ParseDuration(req.Lifetime); err != nil {
				err = apierr.InvalidArgument("Invalid lifetime %q.", req.Lifetime)
				break
			}
		}
		var tok string
		var exp time.Time
		if tok, exp, err = s.generateAccessToken(ctx, name, req.Delegates, req.Scope, life); err == nil {
			out = &iamcredentials.GenerateAccessTokenResponse{AccessToken: tok, ExpireTime: exp.UTC().Format(time.RFC3339)}
		}
	case "generateIdToken":
		var req iamcredentials.GenerateIdTokenRequest
		if err = readJSON(r, &req); err != nil {
			break
		}
		var tok string
		if tok, err = s.generateIDToken(ctx, name, req.Delegates, req.Audience, req.IncludeEmail); err == nil {
			out = &iamcredentials.GenerateIdTokenResponse{Token: tok}
		}
	case "signBlob":
		var req iamcredentials.SignBlobRequest
		if err = readJSON(r, &req); err != nil {
			break
		}
		var payload []byte
		if payload, err = base64.StdEncoding.DecodeString(req.Payload); err != nil {
			err = apierr.InvalidArgument("payload is not valid base64.")
			break
		}
		var kid string
		var sig []byte
		if kid, sig, err = s.signBlobFor(ctx, name, req.Delegates, payload); err == nil {
			out = &iamcredentials.SignBlobResponse{KeyId: kid, SignedBlob: base64.StdEncoding.EncodeToString(sig)}
		}
	case "signJwt":
		var req iamcredentials.SignJwtRequest
		if err = readJSON(r, &req); err != nil {
			break
		}
		var kid, jwt string
		if kid, jwt, err = s.signJwtFor(ctx, name, req.Delegates, req.Payload); err == nil {
			out = &iamcredentials.SignJwtResponse{KeyId: kid, SignedJwt: jwt}
		}
	default:
		notFoundRoute(w, r)
		return
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, out)
}

// credentialsServer is the gRPC google.iam.credentials.v1.IAMCredentials.
type credentialsServer struct {
	credentialspb.UnimplementedIAMCredentialsServer
	s *Service
}

func (c *credentialsServer) GenerateAccessToken(ctx context.Context, req *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
	life := time.Duration(0)
	if req.GetLifetime() != nil {
		life = req.GetLifetime().AsDuration()
	}
	tok, exp, err := c.s.generateAccessToken(ctx, req.GetName(), req.GetDelegates(), req.GetScope(), life)
	if err != nil {
		return nil, err
	}
	return &credentialspb.GenerateAccessTokenResponse{AccessToken: tok, ExpireTime: timestamppb.New(exp)}, nil
}

func (c *credentialsServer) GenerateIdToken(ctx context.Context, req *credentialspb.GenerateIdTokenRequest) (*credentialspb.GenerateIdTokenResponse, error) {
	tok, err := c.s.generateIDToken(ctx, req.GetName(), req.GetDelegates(), req.GetAudience(), req.GetIncludeEmail())
	if err != nil {
		return nil, err
	}
	return &credentialspb.GenerateIdTokenResponse{Token: tok}, nil
}

func (c *credentialsServer) SignBlob(ctx context.Context, req *credentialspb.SignBlobRequest) (*credentialspb.SignBlobResponse, error) {
	kid, sig, err := c.s.signBlobFor(ctx, req.GetName(), req.GetDelegates(), req.GetPayload())
	if err != nil {
		return nil, err
	}
	return &credentialspb.SignBlobResponse{KeyId: kid, SignedBlob: sig}, nil
}

func (c *credentialsServer) SignJwt(ctx context.Context, req *credentialspb.SignJwtRequest) (*credentialspb.SignJwtResponse, error) {
	kid, jwt, err := c.s.signJwtFor(ctx, req.GetName(), req.GetDelegates(), req.GetPayload())
	if err != nil {
		return nil, err
	}
	return &credentialspb.SignJwtResponse{KeyId: kid, SignedJwt: jwt}, nil
}
