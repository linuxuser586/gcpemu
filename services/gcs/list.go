package gcs

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Object listing (FR-GCS-002): prefix, delimiter, matchGlob, startOffset,
// endOffset, versions, includeTrailingDelimiter and pagination.

// listOpts are the objects.list parameters.
type listOpts struct {
	prefix, delimiter      string
	startOffset, endOffset string
	glob                   *regexp.Regexp
	versions               bool
	trailingDelimiter      bool
	max                    int
	// marker resumes after a name (and generation, for versions); a prefix
	// marker skips every name under that prefix.
	marker       string
	markerGen    int64
	markerPrefix bool
}

// listResult is one page of objects and prefixes.
type listResult struct {
	items    []*objectRec
	prefixes []string
	next     string
}

func parseListOpts(q map[string][]string) (*listOpts, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	o := &listOpts{
		prefix:            get("prefix"),
		delimiter:         get("delimiter"),
		startOffset:       get("startOffset"),
		endOffset:         get("endOffset"),
		versions:          get("versions") == "true",
		trailingDelimiter: get("includeTrailingDelimiter") == "true",
		max:               pageSize(get("maxResults"), 1000),
	}
	if g := get("matchGlob"); g != "" {
		re, err := globRegexp(g)
		if err != nil {
			return nil, errInvalid("The matchGlob parameter is invalid: %v", err)
		}
		o.glob = re
	}
	if t := get("pageToken"); t != "" {
		raw, err := decodeToken(t)
		if err != nil || len(raw) < 2 {
			return nil, errInvalid("Invalid page token.")
		}
		switch raw[0] {
		case 'p':
			o.marker, o.markerPrefix = raw[2:], true
		case 'v':
			name, gen, _ := strings.Cut(raw[2:], "\x00")
			o.marker = name
			o.markerGen, _ = strconv.ParseInt(gen, 10, 64)
		default:
			o.marker = raw[2:]
		}
	}
	return o, nil
}

// skip reports whether name/gen precede the page marker or fall outside the
// offsets or glob.
func (o *listOpts) skip(name string, gen int64) bool {
	if o.marker != "" {
		if g := o.group(o.marker); g != "" && strings.HasPrefix(name, g) {
			// The group was already emitted; only its directory object may
			// still be pending when the page ended right after the prefix.
			return !(o.markerPrefix && o.trailingDelimiter && name == g)
		}
		if o.markerPrefix {
			if name < o.marker {
				return true
			}
		} else if name < o.marker || (name == o.marker && (!o.versions || gen <= o.markerGen)) {
			return true
		}
	}
	if o.startOffset != "" && name < o.startOffset {
		return true
	}
	if o.endOffset != "" && name >= o.endOffset {
		return true
	}
	return o.glob != nil && !o.glob.MatchString(name)
}

// list scans one bucket's objects.
func list(tx store.Tx, bucket string, o *listOpts) (*listResult, error) {
	res := &listResult{}
	var cands []*objectRec
	var ierr error
	collect := func(ns string) {
		tx.Scan(ns, objPrefix(bucket, o.prefix), func(_ string, v []byte) bool {
			var rec objectRec
			if ierr = jsonUnmarshal(v, &rec); ierr != nil {
				return false
			}
			cands = append(cands, &rec)
			// Without versions the scan is in name order; stop early once
			// well past a full page (prefix collapsing can only shrink it).
			return o.versions || o.delimiter != "" || len(cands) <= o.max+1 || o.marker != "" || o.startOffset != "" || o.glob != nil
		})
	}
	collect(nsObjects)
	if o.versions {
		collect(nsVersions)
		sort.SliceStable(cands, func(i, j int) bool {
			a, b := cands[i].Object, cands[j].Object
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Generation < b.Generation
		})
	}
	if ierr != nil {
		return nil, ierr
	}
	var (
		lastPrefix string
		last       string // token of the last emitted entry
		count      int
	)
	if o.marker != "" {
		lastPrefix = o.group(o.marker)
	}
	for _, rec := range cands {
		name, gen := rec.Object.Name, rec.Object.Generation
		if o.skip(name, gen) {
			continue
		}
		if p := o.group(name); p != "" {
			if p != lastPrefix {
				if count == o.max {
					res.next = encodeToken(last)
					return res, nil
				}
				res.prefixes = append(res.prefixes, p)
				lastPrefix, last = p, "p:"+p
				count++
			}
			if !(o.trailingDelimiter && name == p) {
				continue
			}
			res.items = append(res.items, rec)
			continue
		}
		if count == o.max {
			res.next = encodeToken(last)
			return res, nil
		}
		res.items = append(res.items, rec)
		if o.versions {
			last = "v:" + name + "\x00" + strconv.FormatInt(gen, 10)
		} else {
			last = "o:" + name
		}
		count++
	}
	return res, nil
}

// group returns the delimiter-collapsed prefix name belongs to, or "".
func (o *listOpts) group(name string) string {
	if o.delimiter == "" || !strings.HasPrefix(name, o.prefix) {
		return ""
	}
	rest := name[len(o.prefix):]
	if i := strings.Index(rest, o.delimiter); i >= 0 {
		return o.prefix + rest[:i+len(o.delimiter)]
	}
	return ""
}

func (s *Service) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.check(r.Context(), "storage.objects.list", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	o, err := parseListOpts(r.URL.Query())
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var (
		res *listResult
		bkt *storage.Bucket
	)
	err = s.env.Store.View(func(tx store.Tx) error {
		brec, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		bkt = brec.Bucket
		res, err = list(tx, bucket, o)
		return err
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	out := &storage.Objects{Kind: "storage#objects", Prefixes: res.prefixes, NextPageToken: res.next}
	base := baseURL(r)
	for _, rec := range res.items {
		out.Items = append(out.Items, renderObject(rec, bkt, base))
	}
	writeJSON(w, http.StatusOK, out)
}

// globRegexp converts a GCS matchGlob pattern to a regexp: "**" matches any
// characters including '/', "*" and "?" do not cross '/', "[...]" is a
// character class and "{a,b}" an alternation.
func globRegexp(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	inAlt := 0
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				return nil, errInvalid("unterminated character class")
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += j + 1
		case '{':
			inAlt++
			b.WriteString("(?:")
		case '}':
			if inAlt == 0 {
				b.WriteString(regexp.QuoteMeta("}"))
				continue
			}
			inAlt--
			b.WriteString(")")
		case ',':
			if inAlt > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if inAlt != 0 {
		return nil, errInvalid("unterminated alternation")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
