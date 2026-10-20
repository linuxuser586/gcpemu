package emutest_test

import (
	"context"
	"testing"

	sqlv1 "google.golang.org/api/sqladmin/v1"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestHTTPOptionsSQLAdmin: one endpoint serves both Cloud SQL Admin
// client libraries, whose paths differ (sql/v1beta4/... and v1/...).
func TestHTTPOptionsSQLAdmin(t *testing.T) {
	inst := emutest.Start(t, []string{"sql"})
	opts, err := inst.HTTPOptions("sqladmin")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	beta, err := sqladmin.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beta.Instances.List("sql-proj").Do(); err != nil {
		t.Errorf("v1beta4 list: %v", err)
	}
	v1, err := sqlv1.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v1.Instances.List("sql-proj").Do(); err != nil {
		t.Errorf("v1 list: %v", err)
	}
}
