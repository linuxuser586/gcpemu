package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// Merged lists (ADR 0027): a list that names no Project, such as Resource
// Manager's list of Projects or a list under projects/-, could cover the
// Projects of every Instance. The front Instance asks itself and each
// link it delegates a Project to, and pages through the answers as one
// list, one Instance after another. A Linked Instance answers with the
// items of the Projects it shares with that link only, and the front
// Instance keeps those of the Projects it delegates to it.

// projectLists matches Resource Manager's lists of Projects, by path, or by
// path on its host in Host mode.
var projectLists = regexp.MustCompile(`^(/cloudresourcemanager)?/v(1/projects|3/projects:search)/?$`)

// mergedPageToken prefixes the page tokens of a merged list.
const mergedPageToken = "gcpemu-merged."

// mergedList reports whether r lists what could be in the Projects of
// every Instance.
func (g *Gateway) mergedList(r *http.Request) bool {
	if r.Method != http.MethodGet || isGRPC(r) || internalPath(r.URL.Path) || len(requestProjects(r)) > 0 {
		return false
	}
	if m := projectLists.FindStringSubmatch(r.URL.Path); m != nil {
		return m[1] != "" || hostOnly(r.Host) == "cloudresourcemanager.googleapis.com"
	}
	p := "/" + strings.TrimPrefix(r.URL.EscapedPath(), "/")
	return strings.Contains(p, "/projects/-/") && g.requestBucket(r) == ""
}

// pageCursor is where a merged list's next page starts: at the page token
// Token of the Instance numbered Source, this one being 0.
type pageCursor struct {
	Source int    `json:"s"`
	Token  string `json:"t,omitempty"`
}

// String is the cursor as a page token.
func (c pageCursor) String() string {
	b, _ := json.Marshal(c)
	return mergedPageToken + base64.RawURLEncoding.EncodeToString(b)
}

// parseCursor parses a merged list's page token; "" starts at the first.
func parseCursor(tok string) (pageCursor, error) {
	var c pageCursor
	if tok == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, mergedPageToken))
	if err != nil || !strings.HasPrefix(tok, mergedPageToken) || json.Unmarshal(b, &c) != nil || c.Source < 0 {
		return c, apierr.InvalidArgument("The page token %q is invalid.", tok)
	}
	return c, nil
}

// serveMerged answers r, a merged list, with a page from this Instance or
// a Linked Instance, whose nextPageToken continues with the next.
func (g *Gateway) serveMerged(w http.ResponseWriter, r *http.Request, l Links) {
	sources := append([]string{""}, l.DelegatedLinks()...)
	c, err := parseCursor(r.URL.Query().Get("pageToken"))
	if err == nil && c.Source >= len(sources) {
		err = apierr.InvalidArgument("The page token %q is invalid.", r.URL.Query().Get("pageToken"))
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	p, err := g.principal(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	for ; ; c = (pageCursor{Source: c.Source + 1}) {
		in := r.Clone(r.Context())
		q := in.URL.Query()
		q.Del("pageToken")
		if c.Token != "" {
			q.Set("pageToken", c.Token)
		}
		in.URL.RawQuery = q.Encode()
		in.RequestURI = in.URL.RequestURI()
		page := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		name := sources[c.Source]
		if name == "" {
			g.serve(page, in)
		} else {
			l.Forward(page, in, name, string(p), func(w http.ResponseWriter, err error) {
				apierr.Write(w, apierr.New(codes.Unavailable, "%v", err))
			}, nil)
		}
		if page.status != http.StatusOK {
			page.writeTo(w)
			return
		}
		var keep func(string) bool // this Instance's own items, all of them
		if name != "" {
			keep = func(id string) bool { to, _ := l.DelegatedTo(id); return to == name }
		}
		body, items, next, err := filterList(page.body.Bytes(), keep)
		if err != nil {
			page.writeTo(w)
			return
		}
		last := c.Source == len(sources)-1
		switch {
		case next != "":
			next = pageCursor{Source: c.Source, Token: next}.String()
		case items == 0 && !last:
			continue
		case !last:
			next = pageCursor{Source: c.Source + 1}.String()
		}
		writeJSON(w, withNextPage(body, next))
		return
	}
}

// serveSharedList answers r, a merged list forwarded over the link from,
// with the items of the Projects shared with it only.
func (g *Gateway) serveSharedList(w http.ResponseWriter, r *http.Request, l Links, from string) {
	page := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
	g.serve(page, r)
	if page.status != http.StatusOK {
		page.writeTo(w)
		return
	}
	body, _, next, err := filterList(page.body.Bytes(), func(id string) bool { return l.Shared(from, id) })
	if err != nil {
		writeErrorFor(w, r, apierr.Internal("The list could not be read: %v", err))
		return
	}
	writeJSON(w, withNextPage(body, next))
}

// filterList keeps, in each list of the JSON object b, the items whose
// Projects all pass keep, dropping those that name none, and drops any
// other array, which could name Projects it cannot tell; a nil keep keeps
// everything. It returns the object, how many items it kept, and its
// nextPageToken.
func filterList(b []byte, keep func(id string) bool) (map[string]json.RawMessage, int, string, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, 0, "", err
	}
	n := 0
	for k, v := range body {
		var list []map[string]any
		if json.Unmarshal(v, &list) != nil {
			if keep != nil && bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) {
				delete(body, k)
			}
			continue
		}
		if keep == nil {
			n += len(list)
			continue
		}
		kept := list[:0]
		for _, item := range list {
			ids := itemProjects(item)
			ok := len(ids) > 0
			for _, id := range ids {
				ok = ok && keep(id)
			}
			if ok {
				kept = append(kept, item)
			}
		}
		n += len(kept)
		body[k], _ = json.Marshal(kept)
	}
	var next string
	_ = json.Unmarshal(body["nextPageToken"], &next)
	return body, n, next, nil
}

// itemProjects returns the Projects a listed item names: by its projectId,
// a name starting projects/P, or a service account's email.
func itemProjects(item map[string]any) []string {
	var ids []string
	if id, ok := item["projectId"].(string); ok {
		ids = addProject(ids, id)
	}
	if name, ok := item["name"].(string); ok && strings.HasPrefix(name, "projects/") {
		if m := projectSegment.FindStringSubmatch(name); m != nil {
			ids = addProject(ids, m[1])
		}
	}
	if email, ok := item["email"].(string); ok {
		ids = addProject(ids, project.OfServiceAccount(email))
	}
	return ids
}

// withNextPage sets body's nextPageToken to next, or removes it for "".
func withNextPage(body map[string]json.RawMessage, next string) map[string]json.RawMessage {
	delete(body, "nextPageToken")
	if next != "" {
		body["nextPageToken"], _ = json.Marshal(next)
	}
	return body
}

// writeJSON writes v as a JSON answer.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// bufferedResponse keeps a response to write it, or not, later.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }

// writeTo writes the response it kept to w.
func (b *bufferedResponse) writeTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
