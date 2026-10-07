package sql

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// Data plane (FR-SQL-001, FR-SQL-004): one container per instance running
// the sql-proxy agent as entrypoint, which runs the image's PostgreSQL. The
// data directory lives in a labelled named volume that survives container
// re-creation (stop/start, emulator restarts with --data-dir) and is
// removed with the instance. The container is attached to the services
// network (agent → emulator), to the external network when ipv4Enabled
// (public IP, reported as PRIMARY) and to the VPC's private services
// network when privateNetwork is set (PRIVATE). Port 5432 is also
// published on the host (127.0.0.1:54320+n) for psql/pgx convenience and
// for Docker VMs on macOS (NFR-PORT-003).

const (
	// superuser is the emulator's internal superuser (Cloud SQL's own
	// internal administrator is also called cloudsqladmin); the API's
	// "postgres" user is not a superuser but a cloudsqlsuperuser member.
	superuser    = "cloudsqladmin"
	hostPortBase = 54320
	readyTimeout = 120 * time.Second
	stopTimeout  = 10 * time.Second
)

// plane returns the runtime and network plane.
func (s *Service) plane(ctx context.Context) (*runtime.Manager, *netplane.Plane, error) {
	if s.env.Containers == nil {
		return nil, nil, newOpError("INTERNAL_ERROR", "Cloud SQL needs a container runtime (Docker or Podman); none is configured")
	}
	p, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return nil, nil, newOpError("INTERNAL_ERROR", "Cloud SQL needs a container runtime (Docker or Podman): %v", err)
	}
	return p.Runtime(), p, nil
}

func containerName(rt *runtime.Manager, project, name string) string {
	return rt.Name("sql", project, name)
}

func volumeName(rt *runtime.Manager, project, name string) string {
	return rt.Name("sql", project, name, "data")
}

func resourceLabel(project, name string) string { return "projects/" + project + "/instances/" + name }

// hostIP is the address host ports are published on.
func (s *Service) hostIP() string {
	b := s.env.Config.Bind
	if b == "" || b == "0.0.0.0" || b == "::" || b == "localhost" {
		return "127.0.0.1"
	}
	return b
}

// dynamicPorts reports whether host ports are chosen by the runtime: when
// a "sql" port is configured as 0, or when the gateway port is dynamic and
// no "sql" base is configured (tests, --port-range).
func (s *Service) dynamicPorts() (base int, dynamic bool) {
	if p, ok := s.env.Config.PortSet("sql"); ok {
		return p, p == 0
	}
	if s.env.Config.Port("gateway") == 0 {
		return 0, true
	}
	return hostPortBase, false
}

// allocHostPort picks the instance's host port: its previous one, else
// the lowest free port from the base that no other instance owns.
func (s *Service) allocHostPort(project, name string, prev int) int {
	if prev > 0 {
		return prev
	}
	base, dynamic := s.dynamicPorts()
	if dynamic {
		// A concrete free port (rather than letting the runtime pick) keeps
		// the address stable across container restarts.
		l, err := s.env.ListenTCP(s.hostIP(), 0)
		if err != nil {
			return 0
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	used := map[int]bool{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(k string, rec *instanceRecord) {
			if k != instKey(project, name) {
				used[rec.HostPort] = true
			}
		})
		return nil
	})
	for p := base; p < base+1000 && p < 65536; p++ {
		if used[p] {
			continue
		}
		if l, err := net.Listen("tcp", net.JoinHostPort(s.hostIP(), strconv.Itoa(p))); err == nil {
			l.Close()
			return p
		}
	}
	return 0
}

// startContainer (re)creates an instance's container from its stored
// record, waits until PostgreSQL accepts connections and records the
// container's addresses. The caller holds the instance lock.
func (s *Service) startContainer(ctx context.Context, project, name string) error {
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return err
	}
	ver, ok := pgVersions[rec.Instance.DatabaseVersion]
	if !ok {
		return newOpError("INVALID_REQUEST", "unsupported database version %s", rec.Instance.DatabaseVersion)
	}
	rt, plane, err := s.plane(ctx)
	if err != nil {
		return err
	}
	if err := rt.EnsureImage(ctx, ver.Image, rt.Offline); err != nil {
		return newOpError("INTERNAL_ERROR", "pull %s: %v", ver.Image, err)
	}
	labels := rt.Labels("sql", resourceLabel(project, name), "postgres")
	vol := volumeName(rt, project, name)
	if err := rt.CreateVolume(ctx, vol, labels); err != nil {
		return err
	}
	cname := containerName(rt, project, name)
	_ = rt.RemoveContainer(ctx, cname, true)

	svc, err := plane.Services(ctx)
	if err != nil {
		return err
	}
	gw, err := plane.Addr(ctx, "gateway")
	if err != nil {
		return err
	}
	bin, err := agent.Binary()
	if err != nil {
		return err
	}
	public := rec.Instance.Settings.IpConfiguration != nil && rec.Instance.Settings.IpConfiguration.Ipv4Enabled
	var ext netplane.Net
	if public {
		if ext, err = plane.External(ctx); err != nil {
			return err
		}
	}
	files := []runtime.File{
		{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Path: bin, Mode: 0o755},
		{Name: "gcpemu/" + proxy.ServerCert, Data: []byte(rec.ServerCertPEM)},
		{Name: "gcpemu/" + proxy.ServerKey, Data: []byte(rec.ServerKeyPEM), Mode: 0o600},
		{Name: "gcpemu/" + proxy.ClientCA, Data: []byte(rec.ClientCAPEM)},
		{Name: "gcpemu/" + proxy.HBAFile, Data: []byte(hbaConf)},
		{Name: "docker-entrypoint-initdb.d/00-gcpemu.sql", Data: []byte(bootstrapSQL(rec))},
	}
	control := fmt.Sprintf("http://%s/sqladmin/_agent/v1/projects/%s/instances/%s", gw, project, name)

	hostPort := s.allocHostPort(project, name, rec.HostPort)
	publicIP := rec.PublicIP
	var id string
	for attempt := 0; ; attempt++ {
		nets := []runtime.Attachment{{Network: svc.Name}}
		if public {
			nets = append(nets, runtime.Attachment{Network: ext.Name, IP: publicIP})
		}
		if rec.PrivateNet != nil && rec.PrivateIP != "" {
			nets = append(nets, runtime.Attachment{Network: rec.PrivateNet.Name, IP: rec.PrivateIP})
		}
		spec := runtime.ContainerSpec{
			Name:       cname,
			Image:      ver.Image,
			Entrypoint: []string{agent.ContainerPath},
			Cmd:        []string{},
			Env: []string{
				agent.EnvVar + "=" + proxy.Name,
				proxy.EnvControl + "=" + control,
				proxy.EnvKey + "=" + rec.AgentKey,
				"PGPORT=" + strconv.Itoa(proxy.InternalPort),
				"POSTGRES_USER=" + superuser,
				"POSTGRES_PASSWORD=" + rec.AgentKey,
				"POSTGRES_DB=postgres",
				"POSTGRES_HOST_AUTH_METHOD=scram-sha-256",
			},
			Labels:   labels,
			Hostname: name,
			Networks: nets,
			Ports:    []runtime.Port{{HostIP: s.hostIP(), HostPort: hostPort, ContainerPort: proxy.PublicPort}},
			Mounts:   []runtime.Mount{{Type: "volume", Source: vol, Target: ver.DataMount}},
			Init:     true,
		}
		id, err = rt.CreateContainer(ctx, spec)
		if err != nil && publicIP != "" && attempt < 3 {
			publicIP = "" // the previous address was taken; let the runtime pick
			continue
		}
		if err != nil {
			return err
		}
		if err = rt.CopyTo(ctx, id, "/", files); err != nil {
			_ = rt.RemoveContainer(ctx, id, true)
			return err
		}
		err = rt.StartContainer(ctx, id)
		if err == nil {
			break
		}
		_ = rt.RemoveContainer(ctx, id, true)
		msg := err.Error()
		portBusy := strings.Contains(msg, "already allocated") || strings.Contains(msg, "address already in use")
		if !portBusy || attempt >= 20 {
			return err
		}
		if hostPort = s.allocHostPort(project, name, 0); hostPort == 0 {
			return err
		}
	}
	c, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if hostPort == 0 {
		addr := c.Ports[fmt.Sprintf("%d/tcp", proxy.PublicPort)]
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			hostPort, _ = strconv.Atoi(addr[i+1:])
		}
	}
	if public {
		publicIP = c.IPs[ext.Name]
	} else {
		publicIP = ""
	}
	if err := s.waitReady(ctx, rt, id, net.JoinHostPort(s.hostIP(), strconv.Itoa(hostPort))); err != nil {
		return err
	}
	return s.updateRecord(project, name, func(r *instanceRecord) error {
		r.HostPort, r.PublicIP = hostPort, publicIP
		r.Instance.IpAddresses = ipMappings(r)
		r.AppliedFlags = flagMap(r.Instance.Settings)
		return nil
	})
}

// ipMappings lists the instance's addresses as reported by the API.
func ipMappings(rec *instanceRecord) []*sqladmin.IpMapping {
	var out []*sqladmin.IpMapping
	if rec.PublicIP != "" {
		out = append(out, &sqladmin.IpMapping{Type: "PRIMARY", IpAddress: rec.PublicIP})
	}
	if rec.PrivateIP != "" {
		out = append(out, &sqladmin.IpMapping{Type: "PRIVATE", IpAddress: rec.PrivateIP})
	}
	return out
}

// waitReady polls the published port with an SSLRequest until the agent
// (which listens only once PostgreSQL is up) answers.
func (s *Service) waitReady(ctx context.Context, rt *runtime.Manager, id, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for i := 0; ; i++ {
		if probe(addr) {
			return nil
		}
		if i%10 == 9 {
			c, err := rt.InspectContainer(ctx, id)
			if err != nil {
				return err
			}
			if !c.Running {
				logs, _ := rt.Logs(context.Background(), id, 30)
				return newOpError("INTERNAL_ERROR", "PostgreSQL container exited (code %d): %s", c.ExitCode, strings.TrimSpace(logs))
			}
		}
		select {
		case <-ctx.Done():
			logs, _ := rt.Logs(context.Background(), id, 30)
			return newOpError("INTERNAL_ERROR", "PostgreSQL did not become ready: %v: %s", ctx.Err(), strings.TrimSpace(logs))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// probe sends an SSLRequest and reports whether a protocol answer came back.
func probe(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(proxy.SSLRequest); err != nil {
		return false
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil {
		return false
	}
	return b[0] == 'S' || b[0] == 'N'
}

// stopContainer stops and removes the container; data stays in the volume.
func (s *Service) stopContainer(ctx context.Context, project, name string) error {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return err
	}
	cname := containerName(rt, project, name)
	_ = rt.StopContainer(ctx, cname, stopTimeout)
	if err := rt.RemoveContainer(ctx, cname, true); err != nil {
		return err
	}
	return s.updateRecord(project, name, func(r *instanceRecord) error {
		r.Instance.IpAddresses = ipMappings(r)
		return nil
	})
}

// restartContainer restarts PostgreSQL (and the agent) in place.
func (s *Service) restartContainer(ctx context.Context, project, name string) error {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return err
	}
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return err
	}
	cname := containerName(rt, project, name)
	if _, err := rt.InspectContainer(ctx, cname); err != nil {
		return s.startContainer(ctx, project, name)
	}
	if err := rt.RestartContainer(ctx, cname, stopTimeout); err != nil {
		return err
	}
	if err := s.waitReady(ctx, rt, cname, net.JoinHostPort(s.hostIP(), strconv.Itoa(rec.HostPort))); err != nil {
		return err
	}
	return nil
}

// running reports whether the instance's container is running.
func (s *Service) running(ctx context.Context, project, name string) bool {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return false
	}
	c, err := rt.InspectContainer(ctx, containerName(rt, project, name))
	return err == nil && c.Running
}

// destroyInstance removes the container, volume, private IP and all stored
// state of an instance.
func (s *Service) destroyInstance(ctx context.Context, project, name string) error {
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return nil
	}
	if rt, _, err := s.plane(ctx); err == nil {
		if err := removeLabelled(ctx, rt, map[string]string{
			runtime.LabelInstance: rt.InstanceID, runtime.LabelService: "sql", runtime.LabelResource: resourceLabel(project, name),
		}); err != nil {
			return err
		}
	} else if rec.HostPort != 0 || rec.PublicIP != "" {
		return err
	}
	if rec.PrivateNet != nil {
		if vpc, ok := s.vpc(); ok {
			_ = vpc.ReleaseIP(ctx, *rec.PrivateNet, ipOwner(project, name))
		}
	}
	if ps, ok := s.peer("iam").(emu.IAMPolicyStore); ok {
		_ = ps.DeletePolicy(ctx, instanceResource(project, name))
	}
	s.removeBackupFiles(project, name)
	return s.env.Store.Update(func(tx store.Tx) error {
		prefix := project + "/" + name + "/"
		for _, ns := range []string{nsDatabases, nsUsers, nsSSLCerts, nsBackups} {
			var keys []string
			tx.Scan(ns, prefix, func(k string, _ []byte) bool { keys = append(keys, k); return true })
			for _, k := range keys {
				if err := tx.Delete(ns, k); err != nil {
					return err
				}
			}
		}
		return tx.Delete(nsInstances, instKey(project, name))
	})
}

// execSQL runs SQL with psql inside the instance's container over the
// unix socket (trust) as user (default: the internal superuser).
func (s *Service) execSQL(ctx context.Context, project, name, db, user, sql string) (string, error) {
	return s.execIn(ctx, project, name, sql, "psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1",
		"-h", proxy.SocketDir, "-p", strconv.Itoa(proxy.InternalPort), "-U", orDefault(user, superuser), "-d", orDefault(db, "postgres"), "-f", "-")
}

// execIn runs a command in the container with stdin; a non-zero exit is an
// ERROR_RDBMS operation error carrying stderr.
func (s *Service) execIn(ctx context.Context, project, name, stdin string, cmd ...string) (string, error) {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return "", err
	}
	var in []byte
	if stdin != "" {
		in = []byte(stdin)
	}
	res, err := rt.Exec(ctx, containerName(rt, project, name), cmd, in)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			return "", errNotRunning()
		}
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		msg = strings.TrimPrefix(msg, "psql:<stdin>:")
		return "", newOpError("ERROR_RDBMS", "%s", msg)
	}
	return string(res.Stdout), nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// hbaConf: the agent reaches PostgreSQL over the unix socket (trust, used
// after IAM authentication and by the emulator's admin commands) and over
// 127.0.0.1 with SCRAM (BUILT_IN users passed through).
const hbaConf = `# gcpemu Cloud SQL
local   all  all                trust
host    all  all  127.0.0.1/32  scram-sha-256
`

// bootstrapSQL creates Cloud SQL's role model on a fresh data directory
// (FR-SQL-002): cloudsqlsuperuser (CREATEDB, CREATEROLE), the "postgres"
// user as its member (not a superuser), the IAM group roles, database
// ownership, and the initial database flags.
func bootstrapSQL(rec *instanceRecord) string {
	var b strings.Builder
	b.WriteString("CREATE ROLE cloudsqlsuperuser WITH CREATEDB CREATEROLE NOLOGIN;\n")
	b.WriteString("CREATE ROLE cloudsqliamuser NOLOGIN;\n")
	b.WriteString("CREATE ROLE cloudsqliamserviceaccount NOLOGIN;\n")
	b.WriteString("CREATE ROLE postgres WITH LOGIN CREATEDB CREATEROLE")
	if rec.RootPassword != "" {
		b.WriteString(" PASSWORD " + quoteLiteral(rec.RootPassword))
	}
	b.WriteString(";\n")
	b.WriteString("GRANT cloudsqlsuperuser TO postgres WITH ADMIN OPTION;\n")
	b.WriteString("GRANT pg_read_all_stats, pg_signal_backend TO cloudsqlsuperuser;\n")
	b.WriteString("ALTER DATABASE postgres OWNER TO cloudsqlsuperuser;\n")
	b.WriteString("ALTER DATABASE template1 OWNER TO cloudsqlsuperuser;\n")
	b.WriteString(alterSystemSQL(nil, pgSettings(flagMap(rec.Instance.Settings))))
	return b.String()
}

// alterSystemSQL moves PostgreSQL settings from old to new.
func alterSystemSQL(old, new map[string]string) string {
	var b strings.Builder
	for _, k := range unionKeys(old, new) {
		v, ok := new[k]
		switch {
		case !ok:
			b.WriteString("ALTER SYSTEM RESET " + quoteIdent(k) + ";\n")
		case old[k] != v || old == nil:
			b.WriteString("ALTER SYSTEM SET " + quoteIdent(k) + " = " + settingValue(k, v) + ";\n")
		}
	}
	return b.String()
}

// listSettings take a list: ALTER SYSTEM needs one literal per element, or
// the whole string becomes a single element (one library named "a,b").
var listSettings = map[string]bool{
	"shared_preload_libraries": true, "local_preload_libraries": true, "session_preload_libraries": true,
	"search_path": true, "temp_tablespaces": true,
}

// settingValue renders a setting's value for ALTER SYSTEM SET.
func settingValue(name, v string) string {
	if !listSettings[name] {
		return quoteLiteral(v)
	}
	var parts []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, quoteLiteral(p))
		}
	}
	if len(parts) == 0 {
		return "''"
	}
	return strings.Join(parts, ", ")
}

// quoteIdent quotes a PostgreSQL identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// quoteLiteral quotes a PostgreSQL string literal (standard_conforming_strings).
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
