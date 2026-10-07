package compute

import (
	"net/http"
	"strconv"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// Regions, zones and the compute project resource, from the embedded
// location catalogue (FR-CORE-021).

// epochStamp is the creationTimestamp GCP reports for regions and zones.
const epochStamp = "1969-12-31T16:00:00.000-08:00"

func regionResource(p string, reg locations.Region) *computev1.Region {
	out := &computev1.Region{
		Kind:              "compute#region",
		Id:                reg.ID(),
		CreationTimestamp: epochStamp,
		Name:              reg.Name,
		Description:       reg.Name,
		Status:            "UP",
		SelfLink:          link("projects/" + p + "/regions/" + reg.Name),
		Quotas:            []*computev1.Quota{},
	}
	for _, z := range reg.ZoneNames() {
		out.Zones = append(out.Zones, link("projects/"+p+"/zones/"+z))
	}
	return out
}

func zoneResource(p, zone string) *computev1.Zone {
	reg, _ := locations.ZoneRegion(zone)
	return &computev1.Zone{
		Kind:                  "compute#zone",
		Id:                    locations.ZoneID(zone),
		CreationTimestamp:     epochStamp,
		Name:                  zone,
		Description:           zone,
		Status:                "UP",
		Region:                link("projects/" + p + "/regions/" + reg),
		SelfLink:              link("projects/" + p + "/zones/" + zone),
		AvailableCpuPlatforms: []string{"Intel Broadwell", "Intel Cascade Lake", "Intel Ice Lake", "AMD Milan", "AMD Genoa"},
	}
}

func (s *Service) listRegions(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.regions.list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []listItem
	for _, reg := range locations.Regions() {
		items = append(items, listItem{key: reg.Name, v: regionResource(p, reg)})
	}
	writeList(w, r, "compute#regionList", "projects/"+p+"/regions", items)
}

func (s *Service) getRegion(w http.ResponseWriter, r *http.Request) {
	p, name := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.regions.get", "projects/"+p+"/regions/"+name); err != nil {
		apierr.Write(w, err)
		return
	}
	reg, ok := locations.Lookup(name)
	if !ok {
		apierr.Write(w, errNotFound("projects/"+p+"/regions/"+name))
		return
	}
	writeJSON(w, http.StatusOK, regionResource(p, reg))
}

func (s *Service) listZones(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.zones.list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []listItem
	for _, z := range locations.Zones() {
		items = append(items, listItem{key: z, v: zoneResource(p, z)})
	}
	writeList(w, r, "compute#zoneList", "projects/"+p+"/zones", items)
}

func (s *Service) getZone(w http.ResponseWriter, r *http.Request) {
	p, z := r.PathValue("project"), r.PathValue("zone")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.zones.get", "projects/"+p+"/zones/"+z); err != nil {
		apierr.Write(w, err)
		return
	}
	if !locations.IsZone(z) {
		apierr.Write(w, errNotFound("projects/"+p+"/zones/"+z))
		return
	}
	writeJSON(w, http.StatusOK, zoneResource(p, z))
}

// getProject implements projects.get: the compute view of a project with
// its default service account.
func (s *Service) getProject(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.projects.get", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	num := project.NumberString(p)
	id, _ := strconv.ParseUint(num, 10, 64)
	writeJSON(w, http.StatusOK, &computev1.Project{
		Kind:                  "compute#project",
		Id:                    id,
		Name:                  p,
		CreationTimestamp:     stamp(s.env.Clock.Now().Add(-24 * 3600 * 1e9)),
		DefaultServiceAccount: num + "-compute@developer.gserviceaccount.com",
		DefaultNetworkTier:    "PREMIUM",
		XpnProjectStatus:      "UNSPECIFIED_XPN_PROJECT_STATUS",
		VmDnsSetting:          "ZONAL_ONLY",
		SelfLink:              link("projects/" + p),
		CommonInstanceMetadata: &computev1.Metadata{
			Kind:        "compute#metadata",
			Fingerprint: labelFingerprint(nil),
		},
		Quotas: []*computev1.Quota{},
	})
}
