package runtime

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"
)

// SaveImages writes a `docker save`-format tarball of the given local
// images to w (GKE preloads node images from the host cache with it).
func (c *Client) SaveImages(ctx context.Context, refs []string, w io.Writer) error {
	resp, err := c.raw(ctx, http.MethodGet, "/images/get", url.Values{"names": refs}, nil, "")
	if err != nil {
		return fmt.Errorf("save images %v: %w", refs, err)
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

// ImageFile pulls ref for platform ("linux/arm64") and returns the contents
// of file in it, read from a container that is created but never started
// (agent.Images).
func (c *Client) ImageFile(ctx context.Context, ref, platform, file string) ([]byte, error) {
	if err := c.pull(ctx, ref, platform); err != nil {
		return nil, err
	}
	id, err := c.CreateContainer(ctx, ContainerSpec{
		Name:     fmt.Sprintf("gcpemu-image-file-%08x", rand.Uint32()),
		Image:    ref,
		Platform: platform,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.RemoveContainer(rctx, id, true)
	}()
	return c.CopyFrom(ctx, id, file)
}
