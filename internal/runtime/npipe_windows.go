package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

// pipeDialer dials the named pipe at addr, e.g. "//./pipe/docker_engine".
// go-winio retries a busy pipe until ctx is done. Its connection can close
// its write side (ending an exec's stdin) only on a message-mode pipe, which
// is what Docker Engine and Docker Desktop serve.
func pipeDialer(addr string) (func(context.Context) (net.Conn, error), error) {
	if err := checkPipeAddr(addr); err != nil {
		return nil, err
	}
	p := strings.ReplaceAll(addr, "/", `\`)
	return func(ctx context.Context) (net.Conn, error) { return winio.DialPipeContext(ctx, p) }, nil
}

// checkPipeAddr accepts only "//<host>/pipe/<name>", what `docker` accepts:
// any other form, such as the two-slash "npipe://./pipe/x", would dial a
// path relative to the working directory (#418).
func checkPipeAddr(addr string) error {
	host, name, ok := splitPipeAddr(strings.TrimLeft(addr, "/"))
	if ok && strings.HasPrefix(addr, "//") && !strings.HasPrefix(addr, "///") {
		return nil
	}
	msg := fmt.Sprintf("malformed runtime endpoint %q: want npipe:////./pipe/<name>", "npipe://"+addr)
	if ok {
		msg += fmt.Sprintf(" (did you mean npipe:////%s/pipe/%s?)", host, name)
	}
	return errors.New(msg)
}

// splitPipeAddr splits "<host>/pipe/<name>".
func splitPipeAddr(s string) (host, name string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] != "pipe" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}
