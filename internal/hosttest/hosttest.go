// Package hosttest holds helpers for tests that drive gcpemu from the host
// through the docker CLI and the clients' environment, for gcpemu's own
// tests only: the Containerized Instance tests and the Pre-release host
// checks.
package hosttest

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// Eventually polls cond until it holds or timeout passes; diag explains a
// failure.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			msg := ""
			if diag != nil {
				msg = "\n" + diag()
			}
			t.Fatalf("timed out after %v waiting for %s%s", timeout, what, msg)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Docker runs the docker CLI and returns its trimmed output.
func Docker(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// SocketArgs returns the docker run args that mount the runtime's socket
// at /var/run/docker.sock, with the group that owns it where the runtime
// runs, which on a VM runtime is inside the VM: on Docker Desktop root (0),
// whatever the host's socket says, or whether the host has one. They turn
// off SELinux labelling, without which Podman machine's SELinux denies the
// container the socket. It skips the test when containers cannot mount the
// socket.
func SocketArgs(t testing.TB) []string {
	t.Helper()
	const sock = "/var/run/docker.sock"
	mount := []string{"-v", sock + ":" + sock, "--security-opt", "label=disable"}
	out, err := exec.Command("docker", append(append([]string{"run", "--rm"}, mount...), "busybox:1.37", "stat", "-c", "%g", sock)...).CombinedOutput()
	if err != nil {
		t.Skipf("containers cannot mount the Docker socket %s: %v\n%s", sock, err, out)
	}
	return append(mount, "--group-add", strings.TrimSpace(string(out)))
}

// containerGateway is the API gateway's port inside the container.
var containerGateway = strconv.Itoa(config.DefaultPorts["gateway"])

// RunEmulator runs the image as an Ephemeral Instance in container name,
// with its API gateway published at 127.0.0.1:gatewayPort (a port docker
// picks when it is ""), extra docker run args and start args. It waits
// until the container is healthy and returns the host address of the API
// gateway. At cleanup it prints the logs of a failed test's container,
// stops it, so the Instance removes its containers and networks, and
// removes it.
func RunEmulator(t testing.TB, name, image, gatewayPort string, args []string, start ...string) string {
	t.Helper()
	run := append([]string{"run", "-d", "--name", name, "-p", "127.0.0.1:" + gatewayPort + ":" + containerGateway}, args...)
	// Before the run: a container that fails to start is still created.
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "80", name).CombinedOutput()
			t.Logf("emulator logs:\n%s", out)
		}
		// An Ephemeral Instance removes its containers and networks on stop.
		_, _ = exec.Command("docker", "stop", "-t", "60", name).CombinedOutput()
		_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput()
	})
	Docker(t, append(append(run, image, "start", "--ephemeral"), start...)...)
	Eventually(t, 3*time.Minute, "emulator healthy", func() bool {
		return Docker(t, "inspect", "-f", "{{.State.Health.Status}}", name) == "healthy"
	}, nil)
	return strings.TrimSpace(strings.Split(Docker(t, "port", name, containerGateway+"/tcp"), "\n")[0])
}

// ParseEnv returns the variables in `gcpemu env` output for a POSIX shell
// (export K='v') or PowerShell ($env:K = 'v').
func ParseEnv(out string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		var k, v string
		var ok bool
		if rest, ps := strings.CutPrefix(line, "$env:"); ps {
			k, v, ok = strings.Cut(rest, " = ")
			if ok && len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
				v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
			}
		} else {
			k, v, ok = strings.Cut(strings.TrimPrefix(line, "export "), "=")
			if ok && len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
				v = strings.ReplaceAll(v[1:len(v)-1], `'\''`, "'")
			}
		}
		if ok && k != "" && !strings.ContainsAny(k, " #") {
			vars[k] = v
		}
	}
	return vars
}

// EnvValue waits for gcpemuEnv, the output of `gcpemu env`, to set key,
// and returns its value.
func EnvValue(t testing.TB, gcpemuEnv func() string, key string) string {
	t.Helper()
	var v string
	Eventually(t, 30*time.Second, "gcpemu env prints "+key, func() bool {
		var ok bool
		v, ok = ParseEnv(gcpemuEnv())[key]
		return ok
	}, gcpemuEnv)
	return v
}

// LANIP returns an IPv4 address of an up, multicast-capable interface
// other than loopback on which this process can send multicast, or skips
// the test: mDNS needs one. macOS's Local Network privacy refuses a
// process the user hasn't allowed, as on GitHub's macOS runners.
func LANIP(t testing.TB) string {
	t.Helper()
	ifs, _ := net.Interfaces()
	var sendErr error
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				if sendErr = sendMulticast(ifi); sendErr == nil {
					return n.IP.String()
				}
			}
		}
	}
	if sendErr != nil {
		t.Skipf("this process cannot send multicast on a LAN interface: %v", sendErr)
	}
	t.Skip("no multicast-capable LAN interface")
	return ""
}

// sendMulticast sends a datagram to the discard port of the mDNS group on
// ifi.
func sendMulticast(ifi net.Interface) error {
	c, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return err
	}
	defer c.Close()
	pc := ipv4.NewPacketConn(c)
	if err := pc.SetMulticastInterface(&ifi); err != nil {
		return err
	}
	_, err = pc.WriteTo([]byte("gcpemu"), nil, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 9})
	return err
}
