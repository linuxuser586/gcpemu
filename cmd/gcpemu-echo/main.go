// Command gcpemu-echo is the Conformance suite's test backend (#859): an
// HTTP server that answers /status/<code> with that status, any other path
// with 200, and every request with a JSON report of the request it
// received, holding only the headers named in ECHO_HEADERS. It listens on
// PORT, 8080 by default, as Cloud Run expects, and is published with each
// release as docker.io/linuxuser586/gcpemu-echo.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Getenv, func(net.Addr) {}); err != nil {
		fmt.Fprintln(os.Stderr, "gcpemu-echo:", err)
		os.Exit(1)
	}
}

// run serves until ctx is done, then shuts down, letting requests in flight
// finish. listening is called with the address once it accepts requests.
func run(ctx context.Context, getenv func(string) string, listening func(net.Addr)) error {
	port := getenv("PORT")
	if port == "" {
		port = "8080"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: newHandler(getenv("ECHO_HEADERS")), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	listening(ln.Addr())

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
