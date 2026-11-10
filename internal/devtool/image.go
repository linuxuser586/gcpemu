package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"
)

// errNotYet tells waitFor to check again.
var errNotYet = errors.New("not yet")

// waitFor calls check every interval until it returns something other than
// errNotYet, ctx ends, or timeout passes; then it returns errNotYet.
func waitFor(ctx context.Context, timeout, interval time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := check(); err != errNotYet {
			return err
		}
		if !time.Now().Add(interval).Before(deadline) {
			return errNotYet
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// withContainer starts a detached container with `docker run -d args...`,
// runs fn with its ID and, however fn ends (Ctrl-C too), stops it with stop.
func withContainer(args []string, stop func(cid string), fn func(ctx context.Context, cid string) error) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	cid, err := output("", "docker", append([]string{"run", "-d"}, args...)...)
	if err != nil {
		return fmt.Errorf("docker run: %v: %s", err, cid)
	}
	defer stop(cid)
	return fn(ctx, cid)
}

// docker runs docker with its output on ours.
func docker(args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	return cmd.Run()
}

// runImageCheck is the Image readiness check (#498). It runs the image,
// already built, the way the Dockerfile's header documents: with the Docker
// socket and its group, so the Runtime Services start too. It fails unless Docker reports the
// container healthy within 120 s. The container isn't --rm, so the logs of
// one that exited can still be printed. It is stopped gracefully, so the
// Instance removes the Managed containers and networks it made, and then
// removed.
func runImageCheck(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: image-check <image>")
	}
	image := args[0]
	const timeout = 120 * time.Second
	gid, err := output("", "docker", "run", "--rm", "-v", "/var/run/docker.sock:/s", "busybox:1.38", "stat", "-c", "%g", "/s")
	if err != nil {
		return fmt.Errorf("the Docker socket's group: %v: %s", err, gid)
	}
	stop := func(cid string) {
		output("", "docker", "stop", "-t", "60", cid)
		output("", "docker", "rm", cid)
	}
	return withContainer([]string{"--pull", "never", "-v", "/var/run/docker.sock:/var/run/docker.sock", "--group-add", gid, image}, stop, func(ctx context.Context, cid string) error {
		fmt.Printf("image-check: waiting up to %d s for %s to report healthy\n", int(timeout.Seconds()), image)
		var reason, health string
		err := waitFor(ctx, timeout, 2*time.Second, func() error {
			state, err := output("", "docker", "inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", cid)
			if err != nil {
				return fmt.Errorf("docker inspect: %v: %s", err, state)
			}
			status, h, _ := strings.Cut(state, " ")
			health = h
			switch {
			case status == "running" && health == "healthy":
				return nil
			case status == "running" && health == "":
				reason = "the image declares no HEALTHCHECK"
			case status == "running":
				return errNotYet
			default:
				reason = "the container is " + status
			}
			return errReported
		})
		switch err {
		case nil:
			fmt.Printf("image-check: %s is healthy\n", image)
			return nil
		case errNotYet:
			reason = fmt.Sprintf("not healthy after %d s (%s)", int(timeout.Seconds()), health)
		case errReported:
		default:
			return err
		}
		fmt.Println("--- docker logs")
		docker("logs", cid)
		fmt.Println("--- health")
		docker("inspect", "--format", "{{json .State.Health}}", cid)
		return fmt.Errorf("%s failed: %s", image, reason)
	})
}

// runEchoImageCheck checks that a built gcpemu-echo image answers on PORT
// (#862).
func runEchoImageCheck(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: echo-image-check <image>")
	}
	image := args[0]
	const want = `503 {"method":"GET","path":"/status/503","headers":{"x-test":"hi"}}`
	stop := func(cid string) { output("", "docker", "rm", "-f", cid) }
	return withContainer([]string{"--pull", "never", "-e", "PORT=9090", "-e", "ECHO_HEADERS=x-test", "-p", "127.0.0.1::9090", image}, stop, func(ctx context.Context, cid string) error {
		ports, err := output("", "docker", "port", cid, "9090/tcp")
		if err != nil {
			return fmt.Errorf("docker port: %v: %s", err, ports)
		}
		addr, _, _ := strings.Cut(ports, "\n")
		client := &http.Client{Timeout: 5 * time.Second}
		var got string
		err = waitFor(ctx, 10*time.Second, 500*time.Millisecond, func() error {
			req, err := http.NewRequestWithContext(ctx, "GET", "http://"+strings.TrimSpace(addr)+"/status/503", nil)
			if err != nil {
				return err
			}
			req.Header.Set("X-Test", "hi")
			resp, err := client.Do(req)
			if err != nil {
				got = "000 " // what curl reported for no answer
				return errNotYet
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			got = fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimRight(string(body), "\n"))
			return nil
		})
		if err != nil && err != errNotYet {
			return err
		}
		if got != want {
			docker("logs", cid)
			fmt.Fprintf(os.Stderr, "echo-image-check: %s answered: %s\n", image, got)
			fmt.Fprintf(os.Stderr, "echo-image-check: want: %s\n", want)
			return errReported
		}
		fmt.Printf("echo-image-check: %s answers on PORT\n", image)
		return nil
	})
}
