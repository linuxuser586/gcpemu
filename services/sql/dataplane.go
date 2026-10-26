package sql

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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

// archiveVolumeName is the volume holding archived WAL and base backups
// (pitr.go).
func archiveVolumeName(rt *runtime.Manager, project, name string) string {
	return rt.Name("sql", project, name, "archive")
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
	return s.startContainerWith(ctx, project, name, nil)
}

// startContainerWith is startContainer with extra files copied into the
// container before it starts (a point-in-time restore bundle).
func (s *Service) startContainerWith(ctx context.Context, project, name string, extra []runtime.File) error {
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return err
	}
	// Instances created before dnsName existed get it here, with a server
	// certificate that names it.
	if ensureDNSName(rec) {
		if err := s.updateRecord(project, name, func(r *instanceRecord) error {
			r.Instance.DnsName, r.ServerCertPEM, r.ServerKeyPEM = rec.Instance.DnsName, rec.ServerCertPEM, rec.ServerKeyPEM
			return nil
		}); err != nil {
			return err
		}
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
	archive := archiveVolumeName(rt, project, name)
	if err := rt.CreateVolume(ctx, archive, labels); err != nil {
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
	bin, err := rt.AgentBinary(ctx)
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
	files = append(files, extra...)
	control := fmt.Sprintf("http://%s/sqladmin/_agent/v1/projects/%s/instances/%s", gw, project, name)

	// The private services network is realised again: after a restart its
	// container network may have been recreated on another Fallback range.
	if rec.PrivateNet != nil && rec.Instance.Settings.IpConfiguration != nil && rec.Instance.Settings.IpConfiguration.PrivateNetwork != "" {
		if vpc, ok := s.vpc(); ok {
			if pn, err := vpc.PrivateServicesNetwork(ctx, rec.Instance.Settings.IpConfiguration.PrivateNetwork); err == nil {
				rec.PrivateNet = &pn
			} else {
				s.env.Log.Warn("sql: realising the private services network; using the recorded one", "instance", project+":"+name, "err", err)
			}
		}
	}
	translated := rec.PrivateNet != nil && rec.PrivateIP != "" && rec.PrivateNet.Translated
	var capAdd []string
	if translated {
		capAdd = []string{"NET_ADMIN"}
	}
	hostPort := s.allocHostPort(project, name, rec.HostPort)
	publicIP := rec.PublicIP
	// On a VM runtime a Containerized Instance publishes the instance's
	// ports for host clients on a loopback address (loopback.go). The
	// address is allocated, and held, until the container has started.
	var lp *loopPublish
	if public && rt.VM.On && s.hostClients() {
		s.loopMu.Lock()
		defer func() {
			if lp != nil {
				s.loopMu.Unlock()
			}
		}()
		lp, err = newLoopPublish(s.containerAliases(rt),
			func(aliases bool) []string { return s.loopCandidates(rec, aliases) },
			func() error { return errNoLoopback(s.loopHolder(project, name, loopbackHost), true) })
		if err != nil {
			s.loopMu.Unlock()
			lp = nil
			return err
		}
	}
	var id string
	for attempt := 0; ; attempt++ {
		nets := []runtime.Attachment{{Network: svc.Name}}
		if public {
			nets = append(nets, runtime.Attachment{Network: ext.Name, IP: publicIP})
		}
		if rec.PrivateNet != nil && rec.PrivateIP != "" {
			a := runtime.Attachment{Network: rec.PrivateNet.Name, IP: rec.PrivateIP}
			if translated {
				a.IP = "" // the private IP is aliased once started (ADR 0004)
			}
			nets = append(nets, a)
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
			Mounts: []runtime.Mount{
				{Type: "volume", Source: vol, Target: ver.DataMount},
				{Type: "volume", Source: archive, Target: proxy.ArchiveDir},
			},
			Init:   true,
			CapAdd: capAdd,
		}
		if lp != nil {
			spec.Ports = append(spec.Ports, lp.ports()...)
		}
		id, err = rt.CreateContainer(ctx, spec)
		if err != nil && public && publicIP != "" && attempt < 3 {
			if publicIP, err = s.publicIPAfter(ctx, rt, ext, project, name, publicIP, err); err != nil {
				return err
			}
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
		if lp != nil {
			from, refused := lp.addr(), lp.refused
			retry, lerr := lp.retry(err)
			if lp.refused && !refused {
				s.aliasRefused.Store(true)
				s.env.Log.Warn("sql: the runtime refuses loopback aliases; using "+loopbackHost+" only", "instance", project+":"+name, "err", err)
			}
			if lerr != nil {
				return lerr
			}
			if retry {
				if lp.addr() != from {
					s.env.Log.Warn("sql: loopback address unavailable; moved", "instance", project+":"+name, "from", from, "to", lp.addr(), "err", err)
				} else {
					s.env.Log.Warn("sql: port 5432 is taken; serving host clients on 3307 only", "instance", project+":"+name, "addr", from, "err", err)
				}
				continue
			}
		}
		msg := strings.ToLower(err.Error())
		if public && publicIP != "" && !hostPortTaken(err, s.hostIP(), hostPort) {
			next, nerr := s.publicIPAfter(ctx, rt, ext, project, name, publicIP, err)
			if nerr != nil {
				return nerr
			}
			if next != publicIP {
				publicIP = next
				continue
			}
		}
		// A dynamic host port is free when picked but bound by the runtime
		// only now, so another process may have taken it in between.
		portBusy := strings.Contains(msg, "already allocated") || runtime.IsAddressInUse(err)
		if !portBusy || attempt >= 20 {
			return err
		}
		if hostPort = s.allocHostPort(project, name, 0); hostPort == 0 {
			return err
		}
	}
	loopIP := ""
	if lp != nil {
		// Recorded before the lock is released, so that the next instance
		// skips the address.
		loopIP = lp.addr()
		err := s.updateRecord(project, name, func(r *instanceRecord) error {
			r.LoopIP = loopIP
			return nil
		})
		s.loopMu.Unlock()
		lp = nil
		if err != nil {
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
	if err := aliasPrivateIP(ctx, rt, rec, id); err != nil {
		return err
	}
	addr, err := s.readyAddr(ctx, rt, plane, svc.Name, id, hostPort)
	if err != nil {
		return err
	}
	if err := s.waitReady(ctx, rt, plane, id, addr); err != nil {
		return err
	}
	switch {
	case loopIP != "": // published with the container
	case public && relayOn(plane) && s.hostClients():
		if loopIP, err = s.exposeLoopback(plane, rec, publicIP); err != nil {
			return err
		}
	default:
		s.closeLoopback(project, name)
	}
	lanPort := s.serveLAN(ctx, plane, rec, publicIP, hostPort)
	return s.updateRecord(project, name, func(r *instanceRecord) error {
		r.HostPort, r.PublicIP, r.LoopIP, r.LANPort = hostPort, publicIP, loopIP, lanPort
		if rec.PrivateNet != nil && r.PrivateNet != nil {
			r.PrivateNet = rec.PrivateNet
		}
		// AppliedFlags is left alone: a fresh volume got the record's flags
		// from bootstrapSQL (its callers record them), an existing one
		// keeps what was applied, and applyFlags catches up from there.
		r.Instance.IpAddresses = ipMappings(r)
		return nil
	})
}

// hostPortTaken reports whether err, from starting a Cloud SQL instance's
// container, says its published host port is taken. The runtime says
// "Address already in use" for a taken container address too; only this
// one names the binding (hostIP:hostPort), and it picks another host port
// without moving the Public IP (#673).
func hostPortTaken(err error, hostIP string, hostPort int) bool {
	return runtime.IsAddressInUse(err) && mentions(strings.ToLower(err.Error()), hostIP, hostPort)
}

// publicIPAfter returns the address a Cloud SQL instance's next start asks
// for after creating or starting its container at the Public IP ip failed
// with cause, by what holds ip on the External network ext (nextPublicIP).
func (s *Service) publicIPAfter(ctx context.Context, rt *runtime.Manager, ext netplane.Net, project, name, ip string, cause error) (string, error) {
	h, held, err := rt.AddrHolder(ctx, ext.Name, ip)
	if err != nil {
		return "", fmt.Errorf("%w (looking up what holds the Public IP %s: %v)", cause, ip, err)
	}
	next, err := nextPublicIP(ip, ext.Subnet, rt.InstanceID, h, held)
	if err != nil {
		return "", fmt.Errorf("%w (%v)", err, cause)
	}
	if next != ip {
		s.env.Log.Warn("sql: public IP taken; using a new one", "instance", project+":"+name, "ip", ip, "holder", h.Name, "err", cause)
	}
	return next, nil
}

// nextPublicIP decides, after a start of a Cloud SQL instance at the
// Public IP ip failed, what its next start asks for (ADR 0035): ip again
// while nothing holds it, and "" (a new address) when ip is outside the
// External network's subnet or holder, a container that is not the
// Instance instanceID's own, holds it. A holder of the Instance's own took
// a reserved address, which no Service may: that is an error, and the
// Public IP never moves for it.
func nextPublicIP(ip, subnet, instanceID string, holder runtime.Container, held bool) (string, error) {
	p, perr := netip.ParsePrefix(subnet)
	a, aerr := netip.ParseAddr(ip)
	switch {
	case perr != nil || aerr != nil || !p.Contains(a):
		return "", nil
	case !held:
		return ip, nil
	case holder.Labels[runtime.LabelInstance] == instanceID:
		return "", fmt.Errorf("the Public IP %s is held by %s, a container of this Instance, which never takes a reserved address", ip, holder.Name)
	}
	return "", nil
}

// ipMappings lists the instance's addresses as reported by the API.
func ipMappings(rec *instanceRecord) []*sqladmin.IpMapping {
	var out []*sqladmin.IpMapping
	if rec.PublicIP != "" {
		ip := rec.PublicIP
		if rec.LoopIP != "" {
			ip = rec.LoopIP // on a VM runtime (loopback.go)
		}
		out = append(out, &sqladmin.IpMapping{Type: "PRIMARY", IpAddress: ip})
	}
	if rec.PrivateIP != "" {
		out = append(out, &sqladmin.IpMapping{Type: "PRIVATE", IpAddress: rec.PrivateIP})
	}
	return out
}

// waitReady polls addr (readyAddr) with an SSLRequest until the agent
// (which listens only once PostgreSQL is up) answers.
func (s *Service) waitReady(ctx context.Context, rt *runtime.Manager, plane *netplane.Plane, id, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for i := 0; ; i++ {
		if probe(ctx, plane, addr) {
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
func probe(ctx context.Context, plane *netplane.Plane, addr string) bool {
	dctx, cancel := context.WithTimeout(ctx, time.Second)
	c, err := plane.DialContext(dctx, "tcp", addr)
	cancel()
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
	s.closeLoopback(project, name)
	s.closeLAN(project, name)
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
	rt, plane, err := s.plane(ctx)
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
	if err := aliasPrivateIP(ctx, rt, rec, cname); err != nil {
		return err
	}
	svc, err := plane.Services(ctx)
	if err != nil {
		return err
	}
	addr, err := s.readyAddr(ctx, rt, plane, svc.Name, cname, rec.HostPort)
	if err != nil {
		return err
	}
	return s.waitReady(ctx, rt, plane, cname, addr)
}

// aliasPrivateIP gives a running container on a Translated private
// services network its private IP (ADR 0004); a restart drops it.
func aliasPrivateIP(ctx context.Context, rt *runtime.Manager, rec *instanceRecord, id string) error {
	if rec.PrivateNet == nil || rec.PrivateIP == "" || !rec.PrivateNet.Translated {
		return nil
	}
	c, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	res, err := rt.Exec(ctx, id, []string{"sh", "-c", rec.PrivateNet.AliasScript(rec.PrivateIP, c.IPs[rec.PrivateNet.Name])}, nil)
	if err == nil && res.ExitCode != 0 {
		err = rec.PrivateNet.AliasError(rt.Info.String(), string(res.Stderr), fmt.Errorf("exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr))
	}
	if err != nil {
		return newOpError("INTERNAL_ERROR", "aliasing private IP %s: %v", rec.PrivateIP, err)
	}
	return nil
}

// readyAddr is where the emulator probes an instance's PostgreSQL: the
// published host port, or inside a container (ADR 0003) or through the VM
// relay (ADR 0005) the container's address on the services network.
func (s *Service) readyAddr(ctx context.Context, rt *runtime.Manager, plane *netplane.Plane, svcNet, id string, hostPort int) (string, error) {
	if rt.Self() == "" && plane.Relay() == nil {
		return net.JoinHostPort(s.hostIP(), strconv.Itoa(hostPort)), nil
	}
	c, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(c.IPs[svcNet], strconv.Itoa(proxy.PublicPort)), nil
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
	s.closeLoopback(project, name)
	s.closeLAN(project, name)
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
// 127.0.0.1 with SCRAM (BUILT_IN users passed through); pg_basebackup
// (pitr.go) uses a local replication connection.
const hbaConf = `# gcpemu Cloud SQL
local   all          all                trust
local   replication  all                trust
host    all          all  127.0.0.1/32  scram-sha-256
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
	b.WriteString(alterSystemSQL(nil, pgSettings(serverFlags(rec.Instance.Settings))))
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
