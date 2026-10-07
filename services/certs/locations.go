package certs

import (
	"context"
	"net/http"

	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// Locations mixin (google.cloud.location.Locations): "global" plus every
// region (FR-CORE-021).

func locationIDs() []string {
	out := []string{"global"}
	for _, r := range locations.Regions() {
		out = append(out, r.Name)
	}
	return out
}

func (s *Service) locPerm(api string) string { return api + ".locations" }

func (s *Service) listLocations(ctx context.Context, api, project string, size int32, token string) (*location.ListLocationsResponse, error) {
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	host := cmHost
	if api == "networksecurity" {
		host = nsHost
	}
	if err := s.env.Auth.Check(ctx, s.locPerm(api)+".list", "//"+host+"/projects/"+project); err != nil {
		return nil, err
	}
	var all []*location.Location
	for _, id := range locationIDs() {
		all = append(all, &location.Location{Name: "projects/" + project + "/locations/" + id, LocationId: id, DisplayName: displayName(id)})
	}
	pg, next, err := page(all, token, size)
	if err != nil {
		return nil, err
	}
	return &location.ListLocationsResponse{Locations: pg, NextPageToken: next}, nil
}

func (s *Service) getLocation(ctx context.Context, api, name string) (*location.Location, error) {
	n, err := parseParent(kCert, name)
	if err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(n.Project); err != nil {
		return nil, err
	}
	host := cmHost
	if api == "networksecurity" {
		host = nsHost
	}
	if err := s.env.Auth.Check(ctx, s.locPerm(api)+".get", "//"+host+"/"+name); err != nil {
		return nil, err
	}
	if n.Location != "global" && !locations.IsRegion(n.Location) {
		return nil, apierr.NotFound("Location %s not found.", name).WithHTTP(http.StatusNotFound)
	}
	return &location.Location{Name: name, LocationId: n.Location, DisplayName: displayName(n.Location)}, nil
}

func displayName(id string) string {
	if id == "global" {
		return "Global"
	}
	if r, ok := locations.Lookup(id); ok {
		return r.Description
	}
	return id
}

// locationsServer serves the Locations mixin over gRPC.
type locationsServer struct {
	location.UnimplementedLocationsServer
	s   *Service
	api string
}

func (l *locationsServer) ListLocations(ctx context.Context, req *location.ListLocationsRequest) (*location.ListLocationsResponse, error) {
	n := req.GetName()
	const p = "projects/"
	if len(n) <= len(p) || n[:len(p)] != p {
		return nil, apierr.InvalidArgument("Invalid name %q.", n)
	}
	return l.s.listLocations(ctx, l.api, n[len(p):], req.GetPageSize(), req.GetPageToken())
}

func (l *locationsServer) GetLocation(ctx context.Context, req *location.GetLocationRequest) (*location.Location, error) {
	return l.s.getLocation(ctx, l.api, req.GetName())
}
