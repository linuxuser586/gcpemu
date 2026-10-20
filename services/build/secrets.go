package build

import (
	"context"
	"maps"
	"slices"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Build secrets (FR-CB-007, FR-INT-016): each availableSecrets.secretManager version
// is read from Secret Manager as the Build's service account when the
// Build starts, and given to the steps that list its env in secretEnv
// (or to every step through options.secretEnv). The payloads are never
// stored in the Build, and are masked in its log. Cloud KMS secrets and
// availableSecrets.inline are Absent.

// secretMask replaces a secret's payload in a Build's log.
const secretMask = "[REDACTED]"

// validateSecrets refuses the secrets a Build cannot have: Cloud KMS ones,
// and a secretEnv that is defined twice, not defined, or not used. The
// texts are those of Google's cloud-build-local (validate.go,
// checkSecrets).
func validateSecrets(b *cloudbuildpb.Build) error {
	if len(b.GetSecrets()) > 0 {
		return apierr.New(codes.Unimplemented, "Build secrets (Cloud KMS) is not emulated; use availableSecrets.secretManager.")
	}
	if len(b.GetAvailableSecrets().GetInline()) > 0 {
		return apierr.New(codes.Unimplemented, "Build availableSecrets.inline (Cloud KMS) is not emulated; use availableSecrets.secretManager.")
	}
	defined := map[string]bool{}
	for i, sec := range b.GetAvailableSecrets().GetSecretManager() {
		if sec.GetEnv() == "" || sec.GetVersionName() == "" {
			return apierr.InvalidArgument("invalid build: availableSecrets.secretManager %d needs a versionName and an env.", i)
		}
		if defined[sec.GetEnv()] {
			return apierr.InvalidArgument("invalid build: secretEnv %q is defined more than once", sec.GetEnv())
		}
		defined[sec.GetEnv()] = true
	}
	used := map[string]bool{}
	for _, se := range b.GetOptions().GetSecretEnv() {
		if used[se] {
			return apierr.InvalidArgument("invalid build: Build uses global secretEnv %q more than once", se)
		}
		used[se] = true
	}
	global := maps.Clone(used)
	for i, st := range b.GetSteps() {
		mine := map[string]bool{}
		for _, se := range st.GetSecretEnv() {
			if global[se] {
				return apierr.InvalidArgument("invalid build: Step %d uses the global secretEnv %q", i, se)
			}
			if mine[se] {
				return apierr.InvalidArgument("invalid build: Step %d uses the secretEnv %q more than once", i, se)
			}
			mine[se], used[se] = true, true
		}
		for _, e := range append(slices.Clone(b.GetOptions().GetEnv()), st.GetEnv()...) {
			k, _, _ := strings.Cut(e, "=")
			if mine[k] || global[k] {
				return apierr.InvalidArgument("invalid build: step %d has secret and non-secret env %q", i, k)
			}
		}
	}
	for _, se := range slices.Sorted(maps.Keys(used)) {
		if !defined[se] {
			return apierr.InvalidArgument("invalid build: secretEnv %q is used without being defined", se)
		}
	}
	for _, se := range slices.Sorted(maps.Keys(defined)) {
		if !used[se] {
			return apierr.InvalidArgument("invalid build: secretEnv %q is defined without being used", se)
		}
	}
	return nil
}

// resolveSecrets reads the Build's availableSecrets.secretManager versions
// as its service account, returning each payload by env, and masks them
// in the Build's log. A version that cannot be read, being disabled or
// destroyed or denied to the account, is a *stepFailure.
func (s *Service) resolveSecrets(ctx context.Context, r *liveBuild, b *cloudbuildpb.Build) (map[string]string, error) {
	secs := b.GetAvailableSecrets().GetSecretManager()
	if len(secs) == 0 {
		return nil, nil
	}
	fail := func(detail string) error {
		s.logs.line(r.ref, "ERROR: "+detail)
		return &stepFailure{outcome{status: cloudbuildpb.Build_FAILURE, detail: detail}}
	}
	svc, ok := s.env.Lookup("secrets")
	acc, ok2 := svc.(emu.SecretAccessor)
	if !ok || !ok2 {
		return nil, fail("failed to access secrets: the emulator's Secret Manager Service (secrets) is not running")
	}
	email, err := serviceAccountEmail(b.GetServiceAccount())
	if err != nil {
		return nil, err
	}
	ctx = emu.WithPrincipal(ctx, emu.Principal("serviceAccount:"+email))
	env := map[string]string{}
	for _, sec := range secs {
		data, _, err := acc.AccessSecretVersion(ctx, sec.GetVersionName())
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fail("failed to access secret version " + sec.GetVersionName() + " for env " + sec.GetEnv() + ": " + apierr.From(err).Message)
		}
		env[sec.GetEnv()] = string(data)
	}
	s.logs.mask(r.ref, slices.Collect(maps.Values(env)))
	return env, nil
}

// secretEnv returns step i's secret variables, KEY=payload: options'
// secretEnv, then the step's.
func secretEnv(b *cloudbuildpb.Build, i int, payloads map[string]string) []string {
	var env []string
	for _, k := range append(slices.Clone(b.GetOptions().GetSecretEnv()), b.GetSteps()[i].GetSecretEnv()...) {
		env = append(env, k+"="+payloads[k])
	}
	return env
}
