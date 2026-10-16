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

// SetBeforeServerStart makes f run before each bring-up touches a cluster's
// server container or volume (exported to the external tests).
func SetBeforeServerStart(svc emu.Service, f func(ctx context.Context, key string)) {
	svc.(*Service).beforeServerStart = f
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
	return "gateway-api-" + gatewayAPIVer + "-" + channel + ".yaml.gz"
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
