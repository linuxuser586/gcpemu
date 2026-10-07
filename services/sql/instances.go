package sql

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Instances (FR-SQL-001): insert, get, list, patch, update, delete,
// restart; stop/start through settings.activationPolicy (ALWAYS/NEVER).

func (s *Service) insertInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project := r.PathValue("project")
	if err := s.env.EnsureProject(project); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(ctx, "cloudsql.instances.create", projectResource(project)); err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := readBody(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.createInstance(ctx, project, raw, "CREATE", nil)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, op)
}

// createInstance validates and stores a new instance and starts its CREATE
// (or CLONE) operation. after, if set, runs inside the operation once the instance is
// running (clone and restore use it to load data).
func (s *Service) createInstance(ctx context.Context, project string, raw map[string]any, opType string, after func(ctx context.Context) error) (*sqladmin.Operation, error) {
	var in sqladmin.DatabaseInstance
	if err := fromMap(raw, &in); err != nil {
		return nil, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	if in.Name == "" {
		return nil, errInvalid("Missing instance name.")
	}
	if !validInstanceName(project, in.Name) {
		return nil, errInvalid("Invalid instance name (%s): it must start with a letter and contain only lowercase letters, numbers and hyphens.", in.Name)
	}
	if in.DatabaseVersion == "" {
		in.DatabaseVersion = defaultDatabaseVersion
	}
	ver, ok := pgVersions[in.DatabaseVersion]
	if !ok {
		if strings.HasPrefix(in.DatabaseVersion, "MYSQL") || strings.HasPrefix(in.DatabaseVersion, "SQLSERVER") {
			return nil, errUnsupported("Cloud SQL for MySQL and SQL Server")
		}
		return nil, errInvalid("Invalid database version %s; supported: %s.", in.DatabaseVersion, strings.Join(databaseVersions(), ", "))
	}
	if in.Region == "" {
		in.Region = defaultRegion
	}
	if !sqlRegions[in.Region] {
		return nil, errInvalid("Invalid region (%s).", in.Region)
	}
	if in.MasterInstanceName != "" || in.ReplicaConfiguration != nil {
		return nil, errUnsupported("Read replicas (FR-SQL-010)")
	}
	rawSettings := child(raw, "settings")
	if in.Settings == nil {
		in.Settings = &sqladmin.Settings{}
	}
	zone := in.Region + "-b"
	if in.Settings.LocationPreference != nil && in.Settings.LocationPreference.Zone != "" {
		zone = in.Settings.LocationPreference.Zone
		if !strings.HasPrefix(zone, in.Region+"-") {
			return nil, errInvalid("Zone (%s) is not in region (%s).", zone, in.Region)
		}
	}
	applySettingsDefaults(in.Settings, rawSettings, zone, true)
	ipc := in.Settings.IpConfiguration
	if ipc.PrivateNetwork != "" {
		ipc.PrivateNetwork = normalizeNetwork(project, ipc.PrivateNetwork)
	}
	if _, err := validateSettings(in.Settings, in.DatabaseVersion); err != nil {
		return nil, err
	}

	rec := &instanceRecord{RootPassword: in.RootPassword, AgentKey: s.env.IDs.Hex(16)}
	in.RootPassword = ""
	in.Kind = "sql#instance"
	in.Project = project
	in.BackendType = "SECOND_GEN"
	in.InstanceType = "CLOUD_SQL_INSTANCE"
	in.ConnectionName = project + ":" + in.Region + ":" + in.Name
	in.GceZone = zone
	in.SelfLink = instanceLink(project, in.Name)
	in.DatabaseInstalledVersion = ver.Installed
	in.MaintenanceVersion = ver.Installed + ".R20260801.00_00"
	in.ServiceAccountEmailAddress = serviceAccountFor(project, s.env.IDs.Hex(3))
	in.CreateTime = s.now()
	in.SqlNetworkArchitecture = "NEW_NETWORK_ARCHITECTURE"
	in.State = "PENDING_CREATE"
	in.IpAddresses = nil
	if err := initPKI(rec, project, in.Name); err != nil {
		return nil, apierr.Internal("generate certificates: %v", err)
	}
	in.ServerCaCert = sslCertOf(rec.ServerCAPEM, project, in.Name)
	in.Etag = etagOf(in.Settings)
	rec.Instance = &in
	rec.AppliedFlags = flagMap(in.Settings) // applied by the bootstrap script

	// Private IP (FR-SQL-004, FR-INT-008) needs the VPC's private services
	// access connection, like GCP.
	if ipc.PrivateNetwork != "" {
		if err := s.allocatePrivateIP(ctx, rec); err != nil {
			return nil, err
		}
	}

	err := s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsInstances, instKey(project, in.Name)) {
			return errInstanceExists()
		}
		if err := store.PutJSON(tx, nsInstances, instKey(project, in.Name), rec); err != nil {
			return err
		}
		if err := store.PutJSON(tx, nsDatabases, childKey(project, in.Name, "postgres"), newDatabase(project, in.Name, "postgres", "", "")); err != nil {
			return err
		}
		return store.PutJSON(tx, nsUsers, childKey(project, in.Name, "postgres"), &userRecord{
			User: newUser(project, in.Name, "postgres", "BUILT_IN"), Password: rec.RootPassword,
		})
	})
	if err != nil {
		if rec.PrivateNet != nil {
			if vpc, ok := s.vpc(); ok {
				_ = vpc.ReleaseIP(ctx, *rec.PrivateNet, ipOwner(project, in.Name))
			}
		}
		return nil, err
	}
	name := in.Name
	op := s.newOp(ctx, project, name, opType)
	return s.runOp(op, true, func(ctx context.Context) error {
		if in.Settings.ActivationPolicy != "NEVER" || after != nil {
			if err := s.startContainer(ctx, project, name); err != nil {
				_ = s.setState(project, name, "FAILED")
				return err
			}
			if after != nil {
				if err := after(ctx); err != nil {
					_ = s.setState(project, name, "FAILED")
					return err
				}
			}
			if in.Settings.ActivationPolicy == "NEVER" {
				if err := s.stopContainer(ctx, project, name); err != nil {
					return err
				}
			}
		}
		return s.setState(project, name, "RUNNABLE")
	}), nil
}

// allocatePrivateIP reserves the instance's address on the VPC's private
// services network.
func (s *Service) allocatePrivateIP(ctx context.Context, rec *instanceRecord) error {
	in := rec.Instance
	vpc, ok := s.vpc()
	if !ok {
		return apierr.FailedPrecondition("Private IP requires the compute service (VPC networks) to be running.").WithLegacy("failedPrecondition")
	}
	pn, err := vpc.PrivateServicesNetwork(ctx, in.Settings.IpConfiguration.PrivateNetwork)
	if err != nil {
		return err
	}
	ip, err := vpc.AllocateIP(ctx, pn, ipOwner(in.Project, in.Name))
	if err != nil {
		return err
	}
	rec.PrivateNet, rec.PrivateIP = &pn, ip
	in.IpAddresses = ipMappings(rec)
	return nil
}

func (s *Service) getInstanceH(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("instance")
	if err := s.check(r.Context(), "cloudsql.instances.get", instanceResource(project, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.loadRecord(project, name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, rec.Instance)
}

func (s *Service) listInstances(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.env.EnsureProject(project); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "cloudsql.instances.list", projectResource(project)); err != nil {
		apierr.Write(w, err)
		return
	}
	items := map[string]*sqladmin.DatabaseInstance{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, project+"/", func(_ string, rec *instanceRecord) {
			if rec.Instance != nil && matchesFilter(rec.Instance, r.URL.Query().Get("filter")) {
				items[rec.Instance.Name] = rec.Instance
			}
		})
		return nil
	})
	page, next, err := paginate(r, sortedKeys(items), items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, &sqladmin.InstancesListResponse{Kind: "sql#instancesList", Items: page, NextPageToken: next})
}

// matchesFilter supports the simple "field:value" / "field=value" terms
// gcloud and the console use (name, state, region, databaseVersion,
// settings.userLabels.KEY), ANDed.
func matchesFilter(in *sqladmin.DatabaseInstance, filter string) bool {
	for _, term := range strings.Fields(filter) {
		if strings.EqualFold(term, "AND") {
			continue
		}
		k, v, ok := strings.Cut(term, ":")
		if !ok {
			k, v, ok = strings.Cut(term, "=")
		}
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		var got string
		switch {
		case k == "name":
			got = in.Name
		case k == "state":
			got = in.State
		case k == "region":
			got = in.Region
		case k == "databaseVersion":
			got = in.DatabaseVersion
		case strings.HasPrefix(k, "settings.userLabels."):
			if in.Settings != nil {
				got = in.Settings.UserLabels[strings.TrimPrefix(k, "settings.userLabels.")]
			}
		default:
			continue
		}
		if got != v {
			return false
		}
	}
	return true
}

func (s *Service) patchInstance(w http.ResponseWriter, r *http.Request) {
	s.modifyInstance(w, r, false)
}

func (s *Service) updateInstance(w http.ResponseWriter, r *http.Request) {
	s.modifyInstance(w, r, true)
}

// modifyInstance implements patch (merge) and update (replace the lists
// in settings). Changes to activationPolicy stop/start the container,
// network changes re-create it, and flag changes are applied with ALTER
// SYSTEM plus a reload or, for restart-required flags, a restart within
// the same operation (FR-SQL-003).
func (s *Service) modifyInstance(w http.ResponseWriter, r *http.Request, replace bool) {
	ctx := r.Context()
	project, name := r.PathValue("project"), r.PathValue("instance")
	if err := s.check(ctx, "cloudsql.instances.update", instanceResource(project, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := readBody(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.loadRecord(project, name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	cur := rec.Instance
	if cur.State == "PENDING_CREATE" || cur.State == "PENDING_DELETE" {
		apierr.Write(w, errInProgress())
		return
	}
	for _, f := range []struct{ key, cur string }{
		{"name", cur.Name}, {"project", cur.Project}, {"region", cur.Region}, {"databaseVersion", cur.DatabaseVersion},
	} {
		if v, ok := raw[f.key].(string); ok && v != "" && v != f.cur {
			if f.key == "databaseVersion" {
				apierr.Write(w, errUnsupported("Major version upgrade"))
			} else {
				apierr.Write(w, errInvalid("The %s field cannot be changed.", f.key))
			}
			return
		}
	}
	allowed := map[string]any{}
	for _, k := range []string{"settings", "failoverReplica", "diskEncryptionConfiguration", "maintenanceVersion"} {
		if v, ok := raw[k]; ok {
			allowed[k] = v
		}
	}
	curMap := toMap(cur)
	rs := child(raw, "settings")
	if replace && rs != nil {
		cs := child(curMap, "settings")
		for _, k := range []string{"databaseFlags", "userLabels", "denyMaintenancePeriods", "authorizedGaeApplications"} {
			if _, ok := rs[k]; !ok {
				delete(cs, k)
			}
		}
		if ip := child(rs, "ipConfiguration"); ip != nil {
			if _, ok := ip["authorizedNetworks"]; !ok {
				delete(child(cs, "ipConfiguration"), "authorizedNetworks")
			}
		}
	}
	var next sqladmin.DatabaseInstance
	if err := fromMap(merge(curMap, allowed), &next); err != nil {
		apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError"))
		return
	}
	if next.Settings == nil {
		next.Settings = &sqladmin.Settings{}
	}
	applySettingsDefaults(next.Settings, rs, cur.GceZone, false)
	if pn := next.Settings.IpConfiguration.PrivateNetwork; pn != "" {
		next.Settings.IpConfiguration.PrivateNetwork = normalizeNetwork(project, pn)
	}
	if _, err := validateSettings(next.Settings, next.DatabaseVersion); err != nil {
		apierr.Write(w, err)
		return
	}
	oldIP, newIP := cur.Settings.IpConfiguration, next.Settings.IpConfiguration
	if oldIP.PrivateNetwork != "" && newIP.PrivateNetwork != oldIP.PrivateNetwork {
		apierr.Write(w, errInvalid("The private network of an instance cannot be changed or removed."))
		return
	}
	next.Settings.SettingsVersion = cur.Settings.SettingsVersion + 1
	next.Etag = etagOf(next.Settings)

	oldAct, newAct := cur.Settings.ActivationPolicy, next.Settings.ActivationPolicy
	netChanged := oldIP.Ipv4Enabled != newIP.Ipv4Enabled || oldIP.PrivateNetwork != newIP.PrivateNetwork
	flagsChanged := !reflect.DeepEqual(flagMap(cur.Settings), flagMap(next.Settings))

	nrec := *rec
	nrec.Instance = &next
	if newIP.PrivateNetwork != "" && rec.PrivateIP == "" {
		if err := s.allocatePrivateIP(ctx, &nrec); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	if err := s.env.Store.Update(func(tx store.Tx) error {
		latest, ok := getInstance(tx, project, name)
		if !ok {
			return errInstanceNotFound()
		}
		if latest.Instance.Settings.SettingsVersion != cur.Settings.SettingsVersion {
			return errInProgress()
		}
		next.State, next.IpAddresses = latest.Instance.State, ipMappings(&nrec)
		nrec.HostPort, nrec.PublicIP, nrec.AppliedFlags = latest.HostPort, latest.PublicIP, latest.AppliedFlags
		return store.PutJSON(tx, nsInstances, instKey(project, name), &nrec)
	}); err != nil {
		apierr.Write(w, err)
		return
	}

	needsWork := oldAct != newAct || (newAct != "NEVER" && (netChanged || flagsChanged))
	op := s.newOp(ctx, project, name, "UPDATE")
	op = s.runOp(op, needsWork, func(ctx context.Context) error {
		switch {
		case newAct == "NEVER" && oldAct != "NEVER":
			return s.stopContainer(ctx, project, name)
		case newAct == "NEVER":
			return nil
		case oldAct == "NEVER" || netChanged:
			if err := s.startContainer(ctx, project, name); err != nil {
				return err
			}
		}
		return s.applyFlags(ctx, project, name)
	})
	writeJSON(w, r, op)
}

// applyFlags brings PostgreSQL's settings in line with settings.databaseFlags.
func (s *Service) applyFlags(ctx context.Context, project, name string) error {
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return err
	}
	want := flagMap(rec.Instance.Settings)
	if reflect.DeepEqual(want, rec.AppliedFlags) || (len(want) == 0 && len(rec.AppliedFlags) == 0) {
		return nil
	}
	if q := alterSystemSQL(pgSettings(rec.AppliedFlags), pgSettings(want)); q != "" {
		if _, err := s.execSQL(ctx, project, name, "postgres", "", q); err != nil {
			return err
		}
	}
	if needsRestart(rec.AppliedFlags, want) {
		if err := s.restartContainer(ctx, project, name); err != nil {
			return err
		}
	} else if _, err := s.execSQL(ctx, project, name, "postgres", "", "SELECT pg_reload_conf();"); err != nil {
		return err
	}
	return s.updateRecord(project, name, func(r *instanceRecord) error {
		r.AppliedFlags = want
		return nil
	})
}

func (s *Service) deleteInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project, name := r.PathValue("project"), r.PathValue("instance")
	if err := s.check(ctx, "cloudsql.instances.delete", instanceResource(project, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, ok := getInstance(tx, project, name)
		if !ok {
			return errInstanceNotFound()
		}
		if rec.Instance.Settings != nil && rec.Instance.Settings.DeletionProtectionEnabled {
			return errInvalid("The instance is protected from deletion. Disable deletion protection on the instance and try again.")
		}
		if rec.Instance.State == "PENDING_DELETE" {
			return errInProgress()
		}
		rec.Instance.State = "PENDING_DELETE"
		return store.PutJSON(tx, nsInstances, instKey(project, name), rec)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	op := s.newOp(ctx, project, name, "DELETE")
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		return s.destroyInstance(ctx, project, name)
	}))
}

func (s *Service) restartInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project, name := r.PathValue("project"), r.PathValue("instance")
	if err := s.check(ctx, "cloudsql.instances.restart", instanceResource(project, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.loadRecord(project, name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op := s.newOp(ctx, project, name, "RESTART")
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		return s.restartContainer(ctx, project, name)
	}))
}

// requireRunnable fails with Cloud SQL's error when the instance is not
// running (stopped, being created or deleted).
func requireRunnable(rec *instanceRecord) error {
	in := rec.Instance
	if in.State == "PENDING_CREATE" || in.State == "PENDING_DELETE" {
		return errInProgress()
	}
	if in.Settings != nil && in.Settings.ActivationPolicy == "NEVER" || in.State == "FAILED" || in.State == "SUSPENDED" {
		return errNotRunning()
	}
	return nil
}
