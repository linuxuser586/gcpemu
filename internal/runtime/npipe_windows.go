package runtime

import (
	"context"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

// pipeDialer dials the named pipe at addr, e.g. "//./pipe/docker_engine".
// go-winio retries a busy pipe until ctx is done.
func pipeDialer(addr string) (func(context.Context) (net.Conn, error), error) {
	p := strings.ReplaceAll(addr, "/", `\`)
	return func(ctx context.Context) (net.Conn, error) { return winio.DialPipeContext(ctx, p) }, nil
}
