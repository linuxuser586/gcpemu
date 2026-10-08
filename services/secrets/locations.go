package secrets

import (
	"context"
	"strings"

	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// locationsServer implements the google.cloud.location.Locations mixin:
// every region, where regional secrets may live.
type locationsServer struct {
	location.UnimplementedLocationsServer
	s *Service
}

func locationProto(p string, r locations.Region) *location.Location {
	return &location.Location{Name: "projects/" + p + "/locations/" + r.Name, LocationId: r.Name, DisplayName: r.Description}
}

func (l *locationsServer) ListLocations(ctx context.Context, req *location.ListLocationsRequest) (*location.ListLocationsResponse, error) {
	p, ok := strings.CutPrefix(req.GetName(), "projects/")
	if !ok || p == "" || strings.Contains(p, "/") {
		return nil, invalidName(req.GetName())
	}
	id, err := l.s.resolveProject(p)
	if err != nil {
		return nil, err
	}
	if err := l.s.check(ctx, "secretmanager.locations.list", resRoot+"projects/"+id); err != nil {
		return nil, err
	}
	var all []*location.Location
	for _, r := range locations.Regions() {
		all = append(all, locationProto(p, r))
	}
	pg, next, err := page(all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &location.ListLocationsResponse{Locations: pg, NextPageToken: next}, nil
}

func (l *locationsServer) GetLocation(ctx context.Context, req *location.GetLocationRequest) (*location.Location, error) {
	parts := strings.Split(req.GetName(), "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "locations" {
		return nil, invalidName(req.GetName())
	}
	id, err := l.s.resolveProject(parts[1])
	if err != nil {
		return nil, err
	}
	if err := l.s.check(ctx, "secretmanager.locations.get", resRoot+"projects/"+id); err != nil {
		return nil, err
	}
	r, ok := locations.Lookup(parts[3])
	if !ok {
		return nil, apierr.NotFound("Location %s not found.", req.GetName())
	}
	return locationProto(parts[1], r), nil
}
