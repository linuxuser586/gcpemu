package iam

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces.
const (
	nsAccounts = "iam/serviceaccounts"     // email → iamv1.ServiceAccount
	nsDeleted  = "iam/serviceaccounts-del" // uniqueId → deletedAccount
	nsKeys     = "iam/keys"                // email/keyId → keyRecord
	nsPolicies = "iam/policies"            // full resource name → iamv1.Policy
	nsParents  = "iam/parents"             // full resource name → parent
	nsRoles    = "iam/roles"               // projects/P/roles/R → iamv1.Role
	nsTokens   = "iam/tokens"              // access token → tokenRecord
	nsProjects = "core/projects"           // owned by emu (FR-CORE-020)
)

// saEtag is the etag GCP reports on service accounts.
const saEtag = "MDEwMjE5MjA="

const iamDomain = "iam.googleapis.com"

// deletedAccount is a soft-deleted service account kept for undelete.
type deletedAccount struct {
	Account    *iamv1.ServiceAccount `json:"account"`
	DeleteTime time.Time             `json:"deleteTime"`
}

var (
	accountIDRE      = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])$`)
	defaultComputeRE = regexp.MustCompile(`^(\d+)-compute@developer\.gserviceaccount\.com$`)
)

// saResource is the full resource name IAM checks use for an account.
func saResource(sa *iamv1.ServiceAccount) string {
	return "//iam.googleapis.com/projects/" + sa.ProjectId + "/serviceAccounts/" + sa.Email
}

// projectResource is the full resource name of a project.
func projectResource(id string) string {
	return "//cloudresourcemanager.googleapis.com/projects/" + id
}

// saEmail builds the email of a user-created service account.
func saEmail(projectID, accountID string) string {
	// Domain-scoped project IDs ("example.com:proj") map to
	// "proj.example.com.iam.gserviceaccount.com".
	if dom, p, ok := strings.Cut(projectID, ":"); ok {
		return accountID + "@" + p + "." + dom + ".iam.gserviceaccount.com"
	}
	return accountID + "@" + projectID + ".iam.gserviceaccount.com"
}

// projectOfEmail derives the project of a service-account email, or "".
func projectOfEmail(email string) string {
	_, dom, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	if p, ok := strings.CutSuffix(dom, ".iam.gserviceaccount.com"); ok {
		return p
	}
	return ""
}

// notFoundSA is GCP's error for an unknown service account.
func notFoundSA() error {
	return apierr.NotFound("Unknown service account").WithReason(iamDomain, "SERVICE_ACCOUNT_NOT_FOUND")
}

// getAccount resolves a service account by email or unique ID inside tx.
// The project's default compute service account always exists.
func (s *Service) getAccount(tx store.Tx, idOrEmail string) (*iamv1.ServiceAccount, bool) {
	var sa iamv1.ServiceAccount
	if strings.Contains(idOrEmail, "@") {
		if store.GetJSON(tx, nsAccounts, strings.ToLower(idOrEmail), &sa) == nil {
			return &sa, true
		}
		if m := defaultComputeRE.FindStringSubmatch(idOrEmail); m != nil {
			if pid, ok := s.projectByNumber(tx, m[1]); ok {
				return s.defaultComputeAccount(pid), true
			}
		}
		return nil, false
	}
	var found *iamv1.ServiceAccount
	tx.Scan(nsAccounts, "", func(_ string, b []byte) bool {
		var a iamv1.ServiceAccount
		if json.Unmarshal(b, &a) == nil && a.UniqueId == idOrEmail {
			found = &a
			return false
		}
		return true
	})
	return found, found != nil
}

// defaultComputeAccount describes PROJECT_NUMBER-compute@developer.gserviceaccount.com.
func (s *Service) defaultComputeAccount(projectID string) *iamv1.ServiceAccount {
	num := project.NumberString(projectID)
	email := num + "-compute@developer.gserviceaccount.com"
	return &iamv1.ServiceAccount{
		Name:           "projects/" + projectID + "/serviceAccounts/" + email,
		ProjectId:      projectID,
		UniqueId:       uniqueIDFor(email),
		Email:          email,
		DisplayName:    "Compute Engine default service account",
		Etag:           saEtag,
		Oauth2ClientId: uniqueIDFor(email),
	}
}

// uniqueIDFor derives a stable 21-digit unique ID for an email.
func uniqueIDFor(email string) string {
	n := project.Number("sa:" + email) // 12 digits
	return fmt.Sprintf("1%08d%012d", n%100_000_000, n)
}

// projectByNumber maps a project number back to a known project ID.
func (s *Service) projectByNumber(tx store.Tx, num string) (string, bool) {
	for _, p := range s.env.Config.Projects {
		if project.NumberString(p) == num {
			return p, true
		}
	}
	if p := s.defaultProject(); project.NumberString(p) == num {
		return p, true
	}
	var id string
	tx.Scan(nsProjects, "", func(k string, _ []byte) bool {
		if project.NumberString(k) == num {
			id = k
			return false
		}
		return true
	})
	return id, id != ""
}

// projectID normalises a project ID or number to the ID.
func (s *Service) projectID(tx store.Tx, p string) string {
	if p != "" && p[0] >= '0' && p[0] <= '9' {
		if id, ok := s.projectByNumber(tx, p); ok {
			return id
		}
	}
	return p
}

// accountFromName parses "projects/P/serviceAccounts/X" (P may be "-").
func accountFromName(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "serviceAccounts" || parts[3] == "" {
		return "", apierr.InvalidArgument("Invalid service account name %q.", name)
	}
	return parts[3], nil
}
