package iam_test

import (
	"context"
	"testing"

	cloudbilling "google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// TestProjectBillingInfo is #130: data "google_project" reads the project
// from Resource Manager and then its billing info from Cloud Billing.
func TestProjectBillingInfo(t *testing.T) {
	inst := startIAM(t)
	c, err := cloudbilling.NewService(context.Background(), option.WithoutAuthentication(),
		option.WithEndpoint(inst.GatewayURL()+"/cloudbilling/"))
	if err != nil {
		t.Fatal(err)
	}
	bi, err := c.Projects.GetBillingInfo("projects/" + testProject).Do()
	if err != nil {
		t.Fatal(err)
	}
	if bi.Name != "projects/"+testProject+"/billingInfo" || bi.ProjectId != testProject || !bi.BillingEnabled || bi.BillingAccountName != "" {
		t.Errorf("billing info = %+v", bi)
	}
	p, err := crmClient(t, inst).Projects.Get(testProject).Do()
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectNumber != project.Number(testProject) {
		t.Errorf("project number = %d, want %d", p.ProjectNumber, project.Number(testProject))
	}
	if _, err := c.Projects.UpdateBillingInfo("projects/"+testProject, &cloudbilling.ProjectBillingInfo{}).Do(); httpCode(err) != 404 {
		t.Errorf("updateBillingInfo: want 404, got %v", err)
	}
}
