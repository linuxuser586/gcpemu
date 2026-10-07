package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
