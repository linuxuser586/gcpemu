package iam

import (
	"net/http"
	"sort"
	"time"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// serveCRM serves the project subset of cloudresourcemanager v1 and v3:
// projects.get/list/search and project IAM policies (FR-IAM-003).
func (s *Service) serveCRM(w http.ResponseWriter, r *http.Request) {
	rt := parseRoute(r.URL.Path)
	if err := s.routeCRM(w, r, rt); err != nil {
		apierr.Write(w, err)
	}
}

func (s *Service) routeCRM(w http.ResponseWriter, r *http.Request, rt route) error {
	if len(rt.segs) == 0 || (rt.segs[0] != "v1" && rt.segs[0] != "v3") {
		notFoundRoute(w, r)
		return nil
	}
	ver := rt.segs[0]
	ctx := r.Context()
	if _, ok := rt.match("", ver, "projects"); ok && r.Method == http.MethodGet && ver == "v1" {
		return s.listProjects(w, r, ver)
	}
	if _, ok := rt.match("search", ver, "projects"); ok && r.Method == http.MethodGet && ver == "v3" {
		return s.listProjects(w, r, ver)
	}
	c, ok := rt.match(rt.verb, ver, "projects", "*")
	if !ok {
		notFoundRoute(w, r)
		return nil
	}
	var pid string
	_ = s.env.Store.View(func(tx store.Tx) error { pid = s.projectID(tx, c[0]); return nil })
	if err := s.env.EnsureProject(pid); err != nil {
		return err
	}
	res := projectResource(pid)
	switch {
	case rt.verb == "" && r.Method == http.MethodGet:
		if err := s.check(ctx, "resourcemanager.projects.get", res); err != nil {
			return err
		}
		writeJSON(w, s.projectInfo(pid, ver))
		return nil
	case r.Method != http.MethodPost:
	case rt.verb == "getIamPolicy":
		var req iamv1.GetIamPolicyRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := s.check(ctx, "resourcemanager.projects.getIamPolicy", res); err != nil {
			return err
		}
		var p *iamv1.Policy
		_ = s.env.Store.View(func(tx store.Tx) error { p = loadPolicy(tx, res); return nil })
		writeJSON(w, p)
		return nil
	case rt.verb == "setIamPolicy":
		var req iamv1.SetIamPolicyRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := s.check(ctx, "resourcemanager.projects.setIamPolicy", res); err != nil {
			return err
		}
		var out *iamv1.Policy
		err := s.env.Store.Update(func(tx store.Tx) error {
			var err error
			out, err = s.storePolicy(tx, res, req.Policy, req.UpdateMask)
			return err
		})
		if err != nil {
			return err
		}
		writeJSON(w, out)
		return nil
	case rt.verb == "testIamPermissions":
		var req iamv1.TestIamPermissionsRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		writeJSON(w, &iamv1.TestIamPermissionsResponse{Permissions: s.TestPermissions(ctx, res, req.Permissions)})
		return nil
	case rt.verb == "getAncestry" && ver == "v1":
		if err := s.check(ctx, "resourcemanager.projects.get", res); err != nil {
			return err
		}
		writeJSON(w, &crmv1.GetAncestryResponse{Ancestor: []*crmv1.Ancestor{{ResourceId: &crmv1.ResourceId{Type: "project", Id: pid}}}})
		return nil
	}
	notFoundRoute(w, r)
	return nil
}

// projectCreateTime returns when the emulator first saw the project.
func (s *Service) projectCreateTime(pid string) string {
	var rec struct {
		CreateTime time.Time `json:"createTime"`
	}
	_ = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsProjects, pid, &rec) })
	if rec.CreateTime.IsZero() {
		rec.CreateTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return rec.CreateTime.UTC().Format(time.RFC3339Nano)
}

// projectInfo renders a project in the v1 or v3 shape.
func (s *Service) projectInfo(pid, ver string) any {
	if ver == "v1" {
		return &crmv1.Project{
			ProjectId:      pid,
			ProjectNumber:  project.Number(pid),
			Name:           pid,
			LifecycleState: "ACTIVE",
			CreateTime:     s.projectCreateTime(pid),
		}
	}
	return &crmv3.Project{
		Name:        "projects/" + project.NumberString(pid),
		ProjectId:   pid,
		DisplayName: pid,
		State:       "ACTIVE",
		CreateTime:  s.projectCreateTime(pid),
		Etag:        `W/"` + project.NumberString(pid) + `"`,
	}
}

// knownProjects lists configured and auto-created projects.
func (s *Service) knownProjects() []string {
	set := map[string]bool{}
	for _, p := range s.env.Config.Projects {
		set[p] = true
	}
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsProjects, "", func(k string, _ []byte) bool { set[k] = true; return true })
		return nil
	})
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (s *Service) listProjects(w http.ResponseWriter, r *http.Request, ver string) error {
	var visible []string
	for _, p := range s.knownProjects() {
		if s.env.Auth.Mode() != config.IAMEnforce || s.Allowed(r.Context(), emu.PrincipalFrom(r.Context()), "resourcemanager.projects.get", projectResource(p)) {
			visible = append(visible, p)
		}
	}
	if ver == "v1" {
		out := &crmv1.ListProjectsResponse{}
		for _, p := range visible {
			out.Projects = append(out.Projects, s.projectInfo(p, ver).(*crmv1.Project))
		}
		writeJSON(w, out)
		return nil
	}
	out := &crmv3.SearchProjectsResponse{}
	for _, p := range visible {
		out.Projects = append(out.Projects, s.projectInfo(p, ver).(*crmv3.Project))
	}
	writeJSON(w, out)
	return nil
}
