package iam_test

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/api/option"
	serviceusage "google.golang.org/api/serviceusage/v1"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// enabledServices lists the project's enabled services the way the google
// provider does.
func enabledServices(t *testing.T, c *serviceusage.Service) []string {
	t.Helper()
	var out []string
	err := c.Services.List("projects/"+testProject).PageSize(5).Filter("state:ENABLED").
		Fields("services/name,nextPageToken").
		Pages(context.Background(), func(r *serviceusage.ListServicesResponse) error {
			for _, s := range r.Services {
				out = append(out, s.Name)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestServiceUsage is #51: google_project_service lists the enabled
// services, batch-enables the missing ones and disables on destroy.
func TestServiceUsage(t *testing.T) {
	inst := startIAM(t)
	c, err := serviceusage.NewService(context.Background(), option.WithoutAuthentication(),
		option.WithEndpoint(inst.GatewayURL()+"/serviceusage/"))
	if err != nil {
		t.Fatal(err)
	}
	parent := "projects/" + project.NumberString(testProject)
	kms := parent + "/services/cloudkms.googleapis.com"
	logging := parent + "/services/logging.googleapis.com"
	container := parent + "/services/container.googleapis.com"

	before := enabledServices(t, c)
	if !slices.Contains(before, container) || slices.Contains(before, kms) {
		t.Fatalf("enabled before = %v", before)
	}
	op, err := c.Services.BatchEnable("projects/"+testProject, &serviceusage.BatchEnableServicesRequest{
		ServiceIds: []string{"cloudkms.googleapis.com", "logging.googleapis.com"},
	}).Do()
	if err != nil || !op.Done {
		t.Fatalf("batchEnable = %+v, %v", op, err)
	}
	after := enabledServices(t, c)
	if !slices.Contains(after, kms) || !slices.Contains(after, logging) || len(after) != len(before)+2 {
		t.Errorf("enabled after batchEnable = %v", after)
	}
	if op, err := c.Operations.Get(op.Name).Do(); err != nil || !op.Done {
		t.Errorf("operations.get = %+v, %v", op, err)
	}

	op, err = c.Services.Disable("projects/"+testProject+"/services/container.googleapis.com",
		&serviceusage.DisableServiceRequest{DisableDependentServices: true}).Do()
	if err != nil || !op.Done {
		t.Fatalf("disable = %+v, %v", op, err)
	}
	s, err := c.Services.Get("projects/" + testProject + "/services/container.googleapis.com").Do()
	if err != nil || s.State != "DISABLED" || s.Name != container || s.Parent != parent {
		t.Errorf("get after disable = %+v, %v", s, err)
	}
	if slices.Contains(enabledServices(t, c), container) {
		t.Error("container is still listed as enabled")
	}
	dis, err := c.Services.List("projects/" + testProject).Filter("state:DISABLED").Do()
	if err != nil || len(dis.Services) != 1 || dis.Services[0].Name != container {
		t.Errorf("disabled = %+v, %v", dis, err)
	}
	if _, err := c.Services.Enable("projects/"+testProject+"/services/container.googleapis.com",
		&serviceusage.EnableServiceRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Services.Get("projects/" + testProject + "/services/container.googleapis.com").Do(); err != nil || s.State != "ENABLED" {
		t.Errorf("get after enable = %+v, %v", s, err)
	}

	if s, err := c.Services.Get("projects/" + testProject + "/services/run.googleapis.com").Do(); err != nil || s.State != "DISABLED" {
		t.Errorf("never enabled = %+v, %v", s, err)
	}
	if _, err := c.Services.Enable("projects/"+testProject+"/services/not a service", nil).Do(); httpCode(err) != 403 {
		t.Errorf("enable of an invalid name: want 403, got %v", err)
	}
	if _, err := c.Services.BatchEnable("projects/"+testProject, &serviceusage.BatchEnableServicesRequest{}).Do(); httpCode(err) != 400 {
		t.Errorf("empty batchEnable: want 400, got %v", err)
	}
}
