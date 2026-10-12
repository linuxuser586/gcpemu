package gke

import (
	"context"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// SetBeforeNodeStart makes f run between each node container's creation
// and its start (exported to the external tests).
func SetBeforeNodeStart(svc emu.Service, f func(ctx context.Context, sn emu.SubnetNet, node, ip string)) {
	svc.(*Service).beforeNodeStart = f
}
