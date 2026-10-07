package gcs

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// XML API (FR-GCS-005): path-style object GET/HEAD/PUT/DELETE (which the Go
// client uses for reads), bucket listing, multipart uploads, POST policy
// uploads, and V4/HMAC request authentication.

const xmlHeader = "<?xml version='1.0' encoding='UTF-8'?>"

// writeXML writes an XML document.
func writeXML(w http.ResponseWriter, code int, v any) {
	b, err := xml.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte("<Error><Code>InternalError</Code></Error>")
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, xmlHeader)
	_, _ = w.Write(b)
}

type xmlErrorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
	Details string   `xml:"Details,omitempty"`
}

// xmlError renders err in the XML API error format.
func xmlError(w http.ResponseWriter, err error) {
	var se *sigError
	if errors.As(err, &se) {
		writeXML(w, se.status, xmlErrorBody{Code: se.code, Message: se.message, Details: se.details})
		return
	}
	e := apierr.From(err)
	status := e.HTTP()
	if status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	body := xmlErrorBody{Message: e.Message}
	switch {
	case status == http.StatusNotFound && strings.HasPrefix(e.Message, "No such object"):
		body.Code, body.Message, body.Details = "NoSuchKey", "The specified key does not exist.", e.Message
	case status == http.StatusNotFound && strings.Contains(e.Message, "bucket"):
		body.Code, body.Message = "NoSuchBucket", "The specified bucket does not exist."
	case status == http.StatusNotFound:
		body.Code = "NoSuchUpload"
	case status == http.StatusPreconditionFailed:
		body.Code = "PreconditionFailed"
	case status == http.StatusForbidden:
		body.Code, body.Details = "AccessDenied", e.Message
	case status == http.StatusConflict && strings.Contains(e.Message, "not empty"):
		body.Code = "BucketNotEmpty"
	case status == http.StatusConflict:
		body.Code = "BucketAlreadyOwnedByYou"
	case status == http.StatusRequestedRangeNotSatisfiable:
		body.Code = "InvalidRange"
	case status == http.StatusBadRequest:
		body.Code = "InvalidArgument"
	default:
		body.Code = "InternalError"
	}
	writeXML(w, status, body)
}

// xmlAuth authenticates signed URLs and HMAC/V4 Authorization headers and
// returns the request context carrying the signer.
func (s *Service) xmlAuth(r *http.Request, bucket, object string) (context.Context, error) {
	p, err := signedURLParams(r.URL.Query())
	if err != nil {
		return nil, err
	}
	if p == nil {
		p = headerSigParams(r)
	}
	if p == nil {
		return r.Context(), nil
	}
	who, err := s.verifyV4(r, p, bucket, object)
	if err != nil {
		return nil, err
	}
	return emu.WithPrincipal(r.Context(), who), nil
}

// serveXML handles path-style XML API requests: /, /BUCKET, /BUCKET/OBJECT.
func (s *Service) serveXML(w http.ResponseWriter, r *http.Request) {
	esc := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	bucketEsc, objEsc, hasObj := strings.Cut(esc, "/")
	bucket, err1 := url.PathUnescape(bucketEsc)
	object, err2 := url.PathUnescape(objEsc)
	if err1 != nil || err2 != nil {
		xmlError(w, errInvalid("Invalid path."))
		return
	}
	if r.Method == http.MethodOptions {
		s.corsPreflight(w, r, bucket)
		return
	}
	ctx, err := s.xmlAuth(r, bucket, object)
	if err != nil {
		xmlError(w, err)
		return
	}
	r = r.WithContext(ctx)
	if bucket != "" {
		s.applyCORS(w, r, bucket)
	}
	q := r.URL.Query()
	switch {
	case bucket == "":
		if r.Method == http.MethodGet {
			s.xmlListBuckets(w, r)
			return
		}
	case !hasObj || object == "":
		switch r.Method {
		case http.MethodGet:
			s.xmlListObjects(w, r, bucket)
			return
		case http.MethodHead:
			if _, err := s.getBucketRec(bucket); err != nil {
				xmlError(w, err)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodPut:
			s.xmlCreateBucket(w, r, bucket)
			return
		case http.MethodDelete:
			if err := s.check(ctx, "storage.buckets.delete", bucketResource(bucket)); err != nil {
				xmlError(w, err)
				return
			}
			if err := s.removeBucket(ctx, bucket, conds{}); err != nil {
				xmlError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
			s.postPolicyUpload(w, r, bucket)
			return
		}
	default:
		switch {
		case q.Get("upload_id") != "" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
			s.uploadChunk(w, r, q.Get("upload_id"))
			return
		case q.Get("upload_id") != "" && r.Method == http.MethodDelete:
			s.cancelUpload(w, r, q.Get("upload_id"))
			return
		case q.Has("uploads") && r.Method == http.MethodPost:
			s.mpuInitiate(w, r, bucket, object)
			return
		case q.Get("uploadId") != "":
			s.mpuServe(w, r, bucket, object, q.Get("uploadId"))
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.xmlGetObject(w, r, bucket, object)
			return
		case http.MethodPut:
			s.xmlPutObject(w, r, bucket, object)
			return
		case http.MethodPost:
			if strings.EqualFold(r.Header.Get("X-Goog-Resumable"), "start") {
				s.xmlStartResumable(w, r, bucket, object)
				return
			}
		case http.MethodDelete:
			gen, err := parseGeneration(q.Get("generation"))
			if err == nil {
				err = s.check(ctx, "storage.objects.delete", objectResource(bucket, object))
			}
			var c conds
			if err == nil {
				c, err = parseXMLConds(r.Header)
			}
			if err == nil {
				err = s.removeObject(ctx, bucket, object, gen, c, baseURL(r))
			}
			if err != nil {
				xmlError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	xmlError(w, apierr.New(codes.Unimplemented, "Method not allowed.").WithHTTP(http.StatusMethodNotAllowed))
}

func (s *Service) xmlGetObject(w http.ResponseWriter, r *http.Request, bucket, object string) {
	if err := s.check(r.Context(), "storage.objects.get", objectResource(bucket, object)); err != nil {
		xmlError(w, err)
		return
	}
	gen, err := parseGeneration(r.URL.Query().Get("generation"))
	if err != nil {
		xmlError(w, err)
		return
	}
	c, err := parseXMLConds(r.Header)
	if err != nil {
		xmlError(w, err)
		return
	}
	rec, _, err := s.readObject(bucket, object, gen, c)
	if err != nil {
		xmlError(w, err)
		return
	}
	if err := s.serveMedia(w, r, rec); err != nil {
		xmlError(w, err)
	}
}

// xmlObjectMeta builds object metadata from XML API request headers
// (x-goog-meta-*, x-amz-meta-*, standard entity headers).
func xmlObjectMeta(h http.Header, name string) *storage.Object {
	meta := &storage.Object{
		Name:               name,
		ContentType:        h.Get("Content-Type"),
		CacheControl:       h.Get("Cache-Control"),
		ContentDisposition: h.Get("Content-Disposition"),
		ContentEncoding:    h.Get("Content-Encoding"),
		ContentLanguage:    h.Get("Content-Language"),
		StorageClass:       h.Get("X-Goog-Storage-Class"),
		CustomTime:         h.Get("X-Goog-Custom-Time"),
	}
	for k, v := range h {
		lk := strings.ToLower(k)
		for _, p := range []string{"x-goog-meta-", "x-amz-meta-"} {
			if strings.HasPrefix(lk, p) && len(v) > 0 {
				if meta.Metadata == nil {
					meta.Metadata = map[string]string{}
				}
				meta.Metadata[lk[len(p):]] = v[0]
			}
		}
	}
	if md5 := h.Get("Content-Md5"); md5 != "" {
		meta.Md5Hash = md5
	}
	return meta
}

// setXMLWriteHeaders sets the response headers of an XML API write.
func setXMLWriteHeaders(w http.ResponseWriter, o *storage.Object) {
	h := w.Header()
	h.Set("X-Goog-Generation", strconv.FormatInt(o.Generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(o.Metageneration, 10))
	h.Add("X-Goog-Hash", "crc32c="+o.Crc32c)
	if o.Md5Hash != "" {
		h.Add("X-Goog-Hash", "md5="+o.Md5Hash)
		if b, err := base64.StdEncoding.DecodeString(o.Md5Hash); err == nil {
			h.Set("ETag", `"`+hex.EncodeToString(b)+`"`)
		}
	}
}

func (s *Service) xmlPutObject(w http.ResponseWriter, r *http.Request, bucket, object string) {
	ctx := r.Context()
	c, err := parseXMLConds(r.Header)
	if err != nil {
		xmlError(w, err)
		return
	}
	src := r.Header.Get("X-Goog-Copy-Source")
	if src == "" {
		src = r.Header.Get("X-Amz-Copy-Source")
	}
	if src != "" {
		s.xmlCopy(w, r, src, bucket, object, c)
		return
	}
	meta := xmlObjectMeta(r.Header, object)
	q := url.Values{}
	if c.GenMatch != nil {
		q.Set("ifGenerationMatch", strconv.FormatInt(*c.GenMatch, 10))
	}
	if c.MetagenMatch != nil {
		q.Set("ifMetagenerationMatch", strconv.FormatInt(*c.MetagenMatch, 10))
	}
	rec, _, err := s.putObject(ctx, bucket, meta, r.Body, r.Header.Get("X-Goog-Hash"), q, baseURL(r))
	if err != nil {
		xmlError(w, err)
		return
	}
	setXMLWriteHeaders(w, rec.Object)
	w.WriteHeader(http.StatusOK)
}

func (s *Service) xmlCopy(w http.ResponseWriter, r *http.Request, src, bucket, object string, c conds) {
	ctx := r.Context()
	srcPath, _ := url.PathUnescape(strings.TrimPrefix(src, "/"))
	sb, so, ok := strings.Cut(srcPath, "/")
	if !ok {
		xmlError(w, errInvalid("Invalid copy source: %s", src))
		return
	}
	if err := s.checkCreate(ctx, bucket, object); err != nil {
		xmlError(w, err)
		return
	}
	q := url.Values{}
	if g := r.Header.Get("X-Goog-Copy-Source-Generation"); g != "" {
		q.Set("sourceGeneration", g)
	}
	srcRec, err := s.copySource(ctx, q, sb, so)
	if err != nil {
		xmlError(w, err)
		return
	}
	var meta *storage.Object
	if strings.EqualFold(r.Header.Get("X-Goog-Metadata-Directive"), "REPLACE") || strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		meta = xmlObjectMeta(r.Header, object)
		meta.Md5Hash = ""
		if meta.ContentType == "" {
			meta.ContentType = srcRec.Object.ContentType
		}
	} else {
		meta, _ = copyMeta(srcRec.Object, nil, object)
	}
	rec, _, err := s.copyTo(ctx, srcRec, bucket, object, meta, c, baseURL(r))
	if err != nil {
		xmlError(w, err)
		return
	}
	setXMLWriteHeaders(w, rec.Object)
	type copyResult struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
	}
	writeXML(w, http.StatusOK, copyResult{LastModified: rec.Object.Updated, ETag: w.Header().Get("ETag")})
}

func (s *Service) xmlStartResumable(w http.ResponseWriter, r *http.Request, bucket, object string) {
	meta := xmlObjectMeta(r.Header, object)
	c, err := parseXMLConds(r.Header)
	if err != nil {
		xmlError(w, err)
		return
	}
	q := url.Values{}
	if c.GenMatch != nil {
		q.Set("ifGenerationMatch", strconv.FormatInt(*c.GenMatch, 10))
	}
	rec, err := s.beginResumable(r.Context(), bucket, meta, q, -1, baseURL(r))
	if err != nil {
		xmlError(w, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("%s/%s/%s?upload_id=%s", baseURL(r), url.PathEscape(bucket), pathEncodeV4(object), rec.ID))
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) xmlCreateBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	proj := r.Header.Get("X-Goog-Project-Id")
	if proj == "" {
		xmlError(w, errInvalid("Missing x-goog-project-id header."))
		return
	}
	b := &storage.Bucket{Name: bucket, StorageClass: r.Header.Get("X-Goog-Storage-Class")}
	if body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(body) > 0 {
		var cfg struct {
			LocationConstraint string `xml:"LocationConstraint"`
			StorageClass       string `xml:"StorageClass"`
		}
		if xml.Unmarshal(body, &cfg) == nil {
			b.Location, b.StorageClass = cfg.LocationConstraint, cfg.StorageClass
		}
	}
	if _, err := s.createBucket(r.Context(), proj, b, false); err != nil {
		xmlError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) xmlListBuckets(w http.ResponseWriter, r *http.Request) {
	proj := r.Header.Get("X-Goog-Project-Id")
	type bucketXML struct {
		Name         string `xml:"Name"`
		CreationDate string `xml:"CreationDate"`
	}
	type result struct {
		XMLName xml.Name    `xml:"ListAllMyBucketsResult"`
		XMLNS   string      `xml:"xmlns,attr"`
		Owner   struct{}    `xml:"Owner"`
		Buckets []bucketXML `xml:"Buckets>Bucket"`
	}
	if proj != "" {
		if err := s.check(r.Context(), "storage.buckets.list", projectResource(proj)); err != nil {
			xmlError(w, err)
			return
		}
	}
	out := result{XMLNS: "http://doc.s3.amazonaws.com/2006-03-01"}
	_ = s.env.Store.View(func(tx store.Tx) error {
		all, err := store.ListJSON[*bucketRec](tx, nsBuckets, "")
		for _, b := range all {
			if proj == "" || b.Project == proj {
				out.Buckets = append(out.Buckets, bucketXML{Name: b.Bucket.Name, CreationDate: b.Bucket.TimeCreated})
			}
		}
		return err
	})
	writeXML(w, http.StatusOK, out)
}

type xmlContents struct {
	Key            string `xml:"Key"`
	Generation     int64  `xml:"Generation"`
	MetaGeneration int64  `xml:"MetaGeneration"`
	LastModified   string `xml:"LastModified"`
	ETag           string `xml:"ETag"`
	Size           uint64 `xml:"Size"`
	StorageClass   string `xml:"StorageClass,omitempty"`
}

type xmlPrefix struct {
	Prefix string `xml:"Prefix"`
}

type xmlListResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	XMLNS                 string        `xml:"xmlns,attr"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	Marker                *string       `xml:"Marker,omitempty"`
	NextMarker            string        `xml:"NextMarker,omitempty"`
	ContinuationToken     string        `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	StartAfter            string        `xml:"StartAfter,omitempty"`
	KeyCount              *int          `xml:"KeyCount,omitempty"`
	MaxKeys               int           `xml:"MaxKeys"`
	Delimiter             string        `xml:"Delimiter,omitempty"`
	IsTruncated           bool          `xml:"IsTruncated"`
	Contents              []xmlContents `xml:"Contents"`
	CommonPrefixes        []xmlPrefix   `xml:"CommonPrefixes"`
}

func md5Hex(o *storage.Object) string {
	b, err := base64.StdEncoding.DecodeString(o.Md5Hash)
	if err != nil || len(b) == 0 {
		return `"` + etagFor(o.Generation, o.Metageneration) + `"`
	}
	return `"` + hex.EncodeToString(b) + `"`
}

func (s *Service) xmlListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.check(r.Context(), "storage.objects.list", bucketResource(bucket)); err != nil {
		xmlError(w, err)
		return
	}
	q := r.URL.Query()
	v2 := q.Get("list-type") == "2"
	o := &listOpts{prefix: q.Get("prefix"), delimiter: q.Get("delimiter"), max: pageSize(q.Get("max-keys"), 1000)}
	if v2 {
		if t := q.Get("continuation-token"); t != "" {
			opts, err := parseListOpts(url.Values{"pageToken": {t}})
			if err != nil {
				xmlError(w, err)
				return
			}
			o.marker, o.markerPrefix = opts.marker, opts.markerPrefix
		} else if sa := q.Get("start-after"); sa != "" {
			o.marker = sa
		}
	} else if m := q.Get("marker"); m != "" {
		o.marker = m
		o.markerPrefix = o.delimiter != "" && strings.HasSuffix(m, o.delimiter)
	}
	var res *listResult
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		var err error
		res, err = list(tx, bucket, o)
		return err
	})
	if err != nil {
		xmlError(w, err)
		return
	}
	out := xmlListResult{
		XMLNS: "http://doc.s3.amazonaws.com/2006-03-01", Name: bucket, Prefix: o.prefix,
		MaxKeys: o.max, Delimiter: o.delimiter, IsTruncated: res.next != "",
	}
	for _, rec := range res.items {
		ob := rec.Object
		out.Contents = append(out.Contents, xmlContents{
			Key: ob.Name, Generation: ob.Generation, MetaGeneration: ob.Metageneration,
			LastModified: ob.Updated, ETag: md5Hex(ob), Size: ob.Size, StorageClass: ob.StorageClass,
		})
	}
	for _, p := range res.prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, xmlPrefix{Prefix: p})
	}
	if v2 {
		n := len(out.Contents) + len(out.CommonPrefixes)
		out.KeyCount = &n
		out.ContinuationToken = q.Get("continuation-token")
		out.StartAfter = q.Get("start-after")
		out.NextContinuationToken = res.next
	} else {
		m := q.Get("marker")
		out.Marker = &m
		if res.next != "" {
			raw, _ := decodeToken(res.next)
			if len(raw) > 2 {
				out.NextMarker = raw[2:]
			}
		}
	}
	writeXML(w, http.StatusOK, out)
}
