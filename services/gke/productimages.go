package gke

import "io/fs"

// ProductImages lists the images a cluster runs that decide how GKE
// behaves, for the weekly Product image scan (#1165): each version's k3s
// image, the system images it preloads, and the images of the embedded
// add-on manifests. It may repeat an image.
func ProductImages() ([]string, error) {
	var out []string
	for _, v := range versions {
		out = append(out, v.Image)
		out = append(out, v.Preload...)
	}
	names, err := fs.Glob(addonFS, "addons/*")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		objs, err := manifest(name[len("addons/"):])
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			out = appendImages(out, o)
		}
	}
	return out, nil
}

// appendImages appends the value of every "image" key in a decoded
// manifest object: those of its containers and init containers.
func appendImages(out []string, v any) []string {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok && k == "image" {
				out = append(out, s)
			} else {
				out = appendImages(out, e)
			}
		}
	case []any:
		for _, e := range v {
			out = appendImages(out, e)
		}
	}
	return out
}
