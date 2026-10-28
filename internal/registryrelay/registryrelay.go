// Package registryrelay implements the Registry relay (ADR 0043). On a VM
// runtime the runtime's daemon pushes and pulls images from inside its VM,
// where the host's loopback addresses don't reach: it is a VM-side client.
// The Registry relay is a Managed container in the VM's host network
// namespace that listens on 127.0.0.1 at the registry's port and pipes
// each connection to the registry's address on the services network,
// which the VM relay serves. VM-side clients then use the same image
// references as Host clients.
package registryrelay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// Suffix names the container (runtime.Manager.Name).
const Suffix = "registryrelay"

const agentName = "registryrelay"

// listening is what the Sidecar agent prints once it listens; Start
// waits for it.
const listening = "registry relay listening on "

// taken is what the Sidecar agent prints when its address is in use.
const taken = "registry relay address in use: "

// ErrPortTaken is returned by Start when something in the VM already
// listens on 127.0.0.1 at the port.
var ErrPortTaken = errors.New("the port is taken inside the runtime's VM")

// readyTimeout bounds how long Start waits for the Sidecar agent to listen.
const readyTimeout = 20 * time.Second

func init() { agent.Register(agentName, runAgent) }

// runAgent is the Sidecar agent: it listens on args[0] and pipes each
// connection to args[1].
func runAgent(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("want LISTEN TARGET, got %q", args)
	}
	l, err := net.Listen("tcp", args[0])
	if err != nil {
		if runtime.IsAddressInUse(err) {
			fmt.Println(taken + args[0])
		}
		return err
	}
	fmt.Println(listening + args[0])
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	serve(l, args[1])
	return nil
}

// serve pipes each connection l accepts to target until l closes.
func serve(l net.Listener, target string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				fmt.Printf("registry relay: dial %s: %v\n", target, err)
				return
			}
			defer u.Close()
			netplane.Splice(c, u)
		}()
	}
}

// Options configure Start.
type Options struct {
	// Port is the registry's port, which the relay listens on at
	// 127.0.0.1 inside the VM.
	Port int
	// Target is the registry's address on the services network.
	Target string
	Log    *slog.Logger
}

// Start removes any Registry relay the Instance left behind and starts a
// new one. It returns ErrPortTaken, and leaves no container, when the
// port is in use inside the VM.
func Start(ctx context.Context, rt *runtime.Manager, o Options) error {
	if err := rt.EnsureImage(ctx, vmrelay.Image, rt.Offline); err != nil {
		return fmt.Errorf("registry relay image %s: %w", vmrelay.Image, err)
	}
	bin, err := rt.AgentBinary(ctx)
	if err != nil {
		return err
	}
	name := rt.Name(Suffix)
	if err := rt.RemoveContainer(ctx, name, true); err != nil {
		return err
	}
	listen := net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Port))
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:        name,
		Image:       vmrelay.Image,
		Entrypoint:  []string{agent.ContainerPath},
		Cmd:         []string{listen, o.Target},
		Env:         []string{agent.EnvVar + "=" + agentName},
		Labels:      rt.Labels("core", Suffix, "relay"),
		HostNetwork: true,
		Init:        true,
	})
	if err != nil {
		return fmt.Errorf("registry relay: %w", err)
	}
	fail := func(err error) error {
		_ = rt.RemoveContainer(context.Background(), id, true)
		return err
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		return fail(fmt.Errorf("registry relay: %w", err))
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		return fail(fmt.Errorf("registry relay: %w", err))
	}
	if err := waitListening(ctx, rt, id); err != nil {
		return fail(err)
	}
	o.Log.Info("Registry relay started", "container", name, "listen", listen, "target", o.Target)
	return nil
}

// waitListening waits until the Sidecar agent in container id prints
// that it listens, or exits.
func waitListening(ctx context.Context, rt *runtime.Manager, id string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for {
		logs, _ := rt.Logs(ctx, id, 20)
		switch {
		case strings.Contains(logs, listening):
			return nil
		case strings.Contains(logs, taken):
			return ErrPortTaken
		}
		ct, err := rt.InspectContainer(ctx, id)
		if err != nil {
			return fmt.Errorf("registry relay: %w", err)
		}
		if !ct.Running && ct.Status == "exited" {
			logs, _ := rt.Logs(context.Background(), id, 20)
			if strings.Contains(logs, taken) {
				return ErrPortTaken
			}
			return fmt.Errorf("registry relay exited (code %d): %s", ct.ExitCode, strings.TrimSpace(logs))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("registry relay did not listen: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Remove removes the Instance's Registry relay, if there is one (Reset).
func Remove(ctx context.Context, rt *runtime.Manager) error {
	return rt.RemoveContainer(ctx, rt.Name(Suffix), true)
}

// Leftovers returns the Registry relays of the Instance whose data dir is
// dir (absolute), as its labels name it, or of every Instance when dir is
// "": one a previous run left behind holds the registry's port on the
// host on OrbStack and Colima.
func Leftovers(ctx context.Context, cl *runtime.Client, dir string) ([]runtime.Container, error) {
	labels := map[string]string{runtime.LabelResource: Suffix}
	if dir != "" {
		labels[runtime.LabelDir] = dir
	}
	return cl.ListContainers(ctx, labels)
}
