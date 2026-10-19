package gke

import (
	"context"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// SetBeforeNodeStart makes f run between each node container's creation
// and its start (exported to the external tests).
func SetBeforeNodeStart(svc emu.Service, f func(ctx context.Context, sn emu.SubnetNet, node, ip string)) {
	svc.(*Service).beforeNodeStart = f
}

// SetClusterStatus writes the status of the cluster with resource name
// name to the Store, leaving its data plane as it is (exported to the
// external tests).
func SetClusterStatus(svc emu.Service, name string, st containerpb.Cluster_Status) error {
	r, err := parseRef(name, "", "", "", "", 1)
	if err != nil {
		return err
	}
	return svc.(*Service).updateCluster(r.key(), func(_ *clusterRecord, c *containerpb.Cluster) error {
		c.Status = st
		return nil
	})
}

// SetBeforeServerStart makes f run before each bring-up touches a cluster's
// server container or volume (exported to the external tests).
func SetBeforeServerStart(svc emu.Service, f func(ctx context.Context, key string)) {
	svc.(*Service).beforeServerStart = f
}

// SetBeforeResetTeardown makes f run in each Reset once it has started a
// new background generation, before it tears data planes down (exported to
// the external tests).
func SetBeforeResetTeardown(svc emu.Service, f func()) {
	svc.(*Service).beforeResetTeardown = f
}

// AddonCRDs returns the names of the CRDs in the embedded add-on manifest
// name (exported to the external tests).
func AddonCRDs(name string) ([]string, error) {
	objs, err := manifest(name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, o := range objs {
		if o["kind"] == "CustomResourceDefinition" {
			_, n, _ := objectPath(o)
			out = append(out, n)
		}
	}
	return out, nil
}

// GatewayAPIManifest is the embedded Gateway API manifest of channel
// ("standard" or "experimental").
func GatewayAPIManifest(channel string) string {
	return gatewayManifestName(channel)
}

// GatewayAPIObject returns the object of kind and name in the embedded
// Gateway API manifest of channel, as the file has it (exported to the
// external tests).
func GatewayAPIObject(channel, kind, name string) map[string]any {
	objs, _ := manifest(GatewayAPIManifest(channel))
	for _, o := range objs {
		if _, n, _ := objectPath(o); o["kind"] == kind && n == name {
			return o
		}
	}
	return nil
}

// BringUpWaits are the waits a cluster bring-up runs one after another,
// each bounded on its own. A server that exits before it is ready is
// started once more (restartExited), so its waits count twice.
var BringUpWaits = map[string]time.Duration{
	"admin kubeconfig, two starts": 2 * adminWait,
	"API server ready, two starts": 2 * apiReadyWait,
	"nodes ready":                  nodeReadyWait,
	"add-on CRDs":                  crdEstablishTimeout,
}
