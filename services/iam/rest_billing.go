package iam

import (
	"net/http"

	cloudbilling "google.golang.org/api/cloudbilling/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// serveBilling serves Cloud Billing v1 projects.getBillingInfo, which the
// google provider's data "google_project" reads after the project itself.
// Every project is billing-enabled with no billing account.
func (s *Service) serveBilling(w http.ResponseWriter, r *http.Request) {
	rt := parseRoute(r.URL.Path)
	c, ok := rt.match("", "v1", "projects", "*", "billingInfo")
	if !ok || r.Method != http.MethodGet {
		notFoundRoute(w, r)
		return
	}
	var pid string
	_ = s.env.Store.View(func(tx store.Tx) error { pid = s.projectID(tx, c[0]); return nil })
	if err := s.env.EnsureProject(pid); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "resourcemanager.projects.get", projectResource(pid)); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, &cloudbilling.ProjectBillingInfo{
		Name:           "projects/" + pid + "/billingInfo",
		ProjectId:      pid,
		BillingEnabled: true,
	})
}
