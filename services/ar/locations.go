package ar

import (
	"context"
	"sort"

	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// arLocations are the locations Artifact Registry serves: every GCP region
// plus the multi-regions us, europe and asia (FR-CORE-021).
var arLocations = map[string]string{
	"us":                      "United States",
	"europe":                  "Europe",
	"asia":                    "Asia",
	"africa-south1":           "Johannesburg",
	"asia-east1":              "Taiwan",
	"asia-east2":              "Hong Kong",
	"asia-northeast1":         "Tokyo",
	"asia-northeast2":         "Osaka",
	"asia-northeast3":         "Seoul",
	"asia-south1":             "Mumbai",
	"asia-south2":             "Delhi",
	"asia-southeast1":         "Singapore",
	"asia-southeast2":         "Jakarta",
	"asia-southeast3":         "Bangkok",
	"australia-southeast1":    "Sydney",
	"australia-southeast2":    "Melbourne",
	"europe-central2":         "Warsaw",
	"europe-north1":           "Finland",
	"europe-north2":           "Stockholm",
	"europe-southwest1":       "Madrid",
	"europe-west1":            "Belgium",
	"europe-west2":            "London",
	"europe-west3":            "Frankfurt",
	"europe-west4":            "Netherlands",
	"europe-west6":            "Zurich",
	"europe-west8":            "Milan",
	"europe-west9":            "Paris",
	"europe-west10":           "Berlin",
	"europe-west12":           "Turin",
	"me-central1":             "Doha",
	"me-central2":             "Dammam",
	"me-west1":                "Tel Aviv",
	"northamerica-northeast1": "Montréal",
	"northamerica-northeast2": "Toronto",
	"northamerica-south1":     "Mexico",
	"southamerica-east1":      "São Paulo",
	"southamerica-west1":      "Santiago",
	"us-central1":             "Iowa",
	"us-east1":                "South Carolina",
	"us-east4":                "Northern Virginia",
	"us-east5":                "Columbus",
	"us-south1":               "Dallas",
	"us-west1":                "Oregon",
	"us-west2":                "Los Angeles",
	"us-west3":                "Salt Lake City",
	"us-west4":                "Las Vegas",
}

// validLocation reports whether loc is an Artifact Registry location.
func validLocation(loc string) bool {
	_, ok := arLocations[loc]
	return ok
}

// checkLocation returns the error GCP returns for an unknown location.
func checkLocation(loc string) error {
	if validLocation(loc) {
		return nil
	}
	return apierr.InvalidArgument("Invalid location: %s", loc).WithReason("artifactregistry.googleapis.com", "INVALID_LOCATION")
}

func locationNames() []string {
	out := make([]string, 0, len(arLocations))
	for k := range arLocations {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// locationsServer implements the google.cloud.location.Locations mixin.
type locationsServer struct {
	location.UnimplementedLocationsServer
	s *Service
}

func locationProto(project, loc string) *location.Location {
	return &location.Location{
		Name:        "projects/" + project + "/locations/" + loc,
		LocationId:  loc,
		DisplayName: arLocations[loc],
	}
}

// ListLocations implements Locations.ListLocations (name is "projects/P").
func (l *locationsServer) ListLocations(ctx context.Context, req *location.ListLocationsRequest) (*location.ListLocationsResponse, error) {
	p, ok := cutPrefix(req.GetName(), "projects/")
	if !ok || p == "" {
		return nil, apierr.InvalidArgument("Invalid resource name %q.", req.GetName())
	}
	if err := l.s.project(ctx, p, "artifactregistry.locations.list"); err != nil {
		return nil, err
	}
	names := locationNames()
	start, size, err := pageBounds(req.GetPageToken(), req.GetPageSize(), len(names))
	if err != nil {
		return nil, err
	}
	resp := &location.ListLocationsResponse{}
	for _, n := range names[start:min(start+size, len(names))] {
		resp.Locations = append(resp.Locations, locationProto(p, n))
	}
	resp.NextPageToken = nextToken(start+size, len(names))
	return resp, nil
}

// GetLocation implements Locations.GetLocation.
func (l *locationsServer) GetLocation(ctx context.Context, req *location.GetLocationRequest) (*location.Location, error) {
	p, loc, err := parseLocationName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := l.s.project(ctx, p, "artifactregistry.locations.get"); err != nil {
		return nil, err
	}
	if !validLocation(loc) {
		return nil, apierr.NotFound("Location %s not found.", req.GetName())
	}
	return locationProto(p, loc), nil
}
