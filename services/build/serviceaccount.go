package build

import (
	"context"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// The Build's service account (FR-CB-010): the one its steps act as at
// the metadata server and the registry proxy (agent.go).

// defaultServiceAccount is the account a Build without serviceAccount
// runs as: the project's Compute Engine default service account, as for
// projects created since Cloud Build stopped defaulting to its legacy
// account.
func defaultServiceAccount(projectID string) string {
	return "projects/" + projectID + "/serviceAccounts/" + project.NumberString(projectID) + "-compute@developer.gserviceaccount.com"
}

// serviceAccountEmail returns the email in a Build's serviceAccount,
// "projects/{project}/serviceAccounts/{email}".
func serviceAccountEmail(name string) (string, error) {
	segs := strings.Split(name, "/")
	if len(segs) != 4 || segs[0] != "projects" || segs[1] == "" || segs[2] != "serviceAccounts" || !strings.Contains(segs[3], "@") {
		return "", apierr.InvalidArgument("invalid build: serviceAccount %q must be of the form projects/{project}/serviceAccounts/{email}.", name)
	}
	return segs[3], nil
}

// checkActAs verifies that the caller may run a Build as the
// serviceAccount it names: iam.serviceAccounts.actAs on the account. A
// Build that names none runs as the default and needs no grant.
func (s *Service) checkActAs(ctx context.Context, projectID string, b *cloudbuildpb.Build) error {
	name := b.GetServiceAccount()
	if name == "" {
		return nil
	}
	email, err := serviceAccountEmail(name)
	if err != nil {
		return err
	}
	p := strings.Split(name, "/")[1]
	if p == "-" {
		p = projectID
	}
	return s.check(ctx, "iam.serviceAccounts.actAs", "//iam.googleapis.com/projects/"+p+"/serviceAccounts/"+email)
}
