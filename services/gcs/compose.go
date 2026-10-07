package gcs

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Compose, copy and rewrite (FR-GCS-002).

const maxComposeSources = 32

func (s *Service) composeObject(w http.ResponseWriter, r *http.Request, bucket, name string) {
	ctx := r.Context()
	if err := s.checkCreate(ctx, bucket, name); err != nil {
		apierr.Write(w, err)
		return
	}
	var req storage.ComposeRequest
	if err := decodeJSON(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	if len(req.SourceObjects) == 0 {
		apierr.Write(w, errRequired("sourceObjects"))
		return
	}
	if len(req.SourceObjects) > maxComposeSources {
		apierr.Write(w, errInvalid("The number of source components provided (%d) exceeds the maximum (%d)", len(req.SourceObjects), maxComposeSources))
		return
	}
	c, err := parseConds(r.URL.Query(), "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	for _, src := range req.SourceObjects {
		if err := s.check(ctx, "storage.objects.get", objectResource(bucket, src.Name)); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	// Resolve the sources.
	var (
		srcs       []*objectRec
		components int64
	)
	err = s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		for _, src := range req.SourceObjects {
			rec, _, err := loadObject(tx, bucket, src.Name, src.Generation)
			if err != nil {
				return err
			}
			if rec == nil {
				return errObjectNotFound(bucket, src.Name)
			}
			if p := src.ObjectPreconditions; p != nil && p.IfGenerationMatch != 0 && p.IfGenerationMatch != rec.Object.Generation {
				return errPrecondition()
			}
			cc := rec.Object.ComponentCount
			if cc == 0 {
				cc = 1
			}
			components += cc
			srcs = append(srcs, rec)
		}
		return nil
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if components > 1024 {
		apierr.Write(w, errInvalid("The total component count of the composite object (%d) exceeds the maximum (1024).", components))
		return
	}
	bw, err := s.blobs.newWriter()
	if err != nil {
		apierr.Write(w, err)
		return
	}
	for _, src := range srcs {
		f, err := s.blobs.open(src.Blob)
		if err != nil {
			bw.abort()
			apierr.Write(w, apierr.Internal("source data unavailable: %v", err))
			return
		}
		_, err = bw.ReadFrom(f)
		f.Close()
		if err != nil {
			bw.abort()
			apierr.Write(w, err)
			return
		}
	}
	info, err := bw.commit()
	if err != nil {
		apierr.Write(w, err)
		return
	}
	meta := req.Destination
	if meta == nil {
		meta = &storage.Object{}
	}
	meta.Name = name
	if meta.ContentType == "" {
		meta.ContentType = srcs[0].Object.ContentType
	}
	if meta.Crc32c != "" && meta.Crc32c != info.crcB64() {
		s.blobs.remove(info.ID)
		apierr.Write(w, errInvalid("Provided CRC32C %q doesn't match calculated CRC32C %q.", meta.Crc32c, info.crcB64()))
		return
	}
	wr := &writeReq{bucket: bucket, meta: meta, blob: info, conds: c, componentCount: components, base: baseURL(r)}
	if req.DeleteSourceObjects {
		wr.extra = func(tx store.Tx, brec *bucketRec, now time.Time) ([]event, []string, error) {
			var evs []event
			var blobs []string
			for _, src := range srcs {
				if src.Object.Name == name {
					continue
				}
				cur, live, err := loadObject(tx, bucket, src.Object.Name, src.Object.Generation)
				if err != nil || cur == nil {
					continue
				}
				if live {
					ev, blob, err := retire(tx, cur, brec.Bucket, now, 0)
					if err != nil {
						return nil, nil, err
					}
					evs = append(evs, ev)
					if blob != "" {
						blobs = append(blobs, blob)
					}
				}
			}
			return evs, blobs, nil
		}
	}
	rec, bkt, err := s.commitObject(ctx, wr)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderObject(rec, bkt, baseURL(r)))
}

// copySource resolves the source of a copy/rewrite with its preconditions.
func (s *Service) copySource(ctx context.Context, q url.Values, bucket, name string) (*objectRec, error) {
	if err := s.check(ctx, "storage.objects.get", objectResource(bucket, name)); err != nil {
		return nil, err
	}
	gen, err := parseGeneration(q.Get("sourceGeneration"))
	if err != nil {
		return nil, err
	}
	sc, err := parseConds(q, "ifSource")
	if err != nil {
		return nil, err
	}
	rec, _, err := s.readObject(bucket, name, gen, conds{})
	if err != nil {
		return nil, err
	}
	if err := sc.check(rec.Object, false); err != nil {
		return nil, err
	}
	return rec, nil
}

// copyMeta returns destination metadata: the request body when it carries
// metadata, otherwise the source's.
func copyMeta(src *storage.Object, body []byte, dstName string) (*storage.Object, error) {
	var meta storage.Object
	if len(body) > 0 {
		if err := json.Unmarshal(body, &meta); err != nil {
			return nil, errInvalid("Invalid JSON payload received. %v", err)
		}
	}
	hasMeta := meta.ContentType != "" || meta.Metadata != nil || meta.CacheControl != "" || meta.ContentDisposition != "" ||
		meta.ContentEncoding != "" || meta.ContentLanguage != "" || meta.CustomTime != ""
	if !hasMeta {
		meta = storage.Object{
			ContentType:        src.ContentType,
			CacheControl:       src.CacheControl,
			ContentDisposition: src.ContentDisposition,
			ContentEncoding:    src.ContentEncoding,
			ContentLanguage:    src.ContentLanguage,
			CustomTime:         src.CustomTime,
			Metadata:           src.Metadata,
			StorageClass:       meta.StorageClass,
		}
	}
	if meta.ContentType == "" {
		meta.ContentType = src.ContentType
	}
	meta.Name = dstName
	meta.Md5Hash, meta.Crc32c = "", ""
	return &meta, nil
}

// copyTo creates the destination object as a hard link of the source.
func (s *Service) copyTo(ctx context.Context, src *objectRec, dstBucket, dstName string, meta *storage.Object, c conds, base string) (*objectRec, *storage.Bucket, error) {
	id, err := s.blobs.link(src.Blob)
	if err != nil {
		return nil, nil, apierr.Internal("copy object data: %v", err)
	}
	info := blobInfoOf(src.Object, id)
	wr := &writeReq{bucket: dstBucket, meta: meta, blob: info, conds: c, componentCount: src.Object.ComponentCount, base: base}
	return s.commitObject(ctx, wr)
}

// blobInfoOf rebuilds blob checksums from stored object metadata.
func blobInfoOf(o *storage.Object, id string) *blobInfo {
	info := &blobInfo{ID: id, Size: int64(o.Size)}
	info.MD5, _ = base64.StdEncoding.DecodeString(o.Md5Hash)
	if b, err := base64.StdEncoding.DecodeString(o.Crc32c); err == nil && len(b) == 4 {
		info.CRC = binary.BigEndian.Uint32(b)
	}
	return info
}

func (s *Service) copyObject(w http.ResponseWriter, r *http.Request, srcBucket, srcName, dstBucket, dstName string) {
	ctx := r.Context()
	q := r.URL.Query()
	if err := s.checkCreate(ctx, dstBucket, dstName); err != nil {
		apierr.Write(w, err)
		return
	}
	src, err := s.copySource(ctx, q, srcBucket, srcName)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	meta, err := copyMeta(src.Object, body, dstName)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, bkt, err := s.copyTo(ctx, src, dstBucket, dstName, meta, c, baseURL(r))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderObject(rec, bkt, baseURL(r)))
}

// rewriteToken is the state carried between rewrite calls.
type rewriteToken struct {
	SrcBucket string `json:"b"`
	SrcName   string `json:"o"`
	SrcGen    int64  `json:"g"`
	Done      int64  `json:"d"`
}

// defaultRewriteChunk is how many bytes one rewrite call reports as copied
// when the caller does not set maxBytesRewrittenPerCall.
var defaultRewriteChunk int64 = 64 << 20

func (s *Service) rewriteObject(w http.ResponseWriter, r *http.Request, srcBucket, srcName, dstBucket, dstName string) {
	ctx := r.Context()
	q := r.URL.Query()
	if err := s.checkCreate(ctx, dstBucket, dstName); err != nil {
		apierr.Write(w, err)
		return
	}
	var tok rewriteToken
	if t := q.Get("rewriteToken"); t != "" {
		raw, err := decodeToken(t)
		if err != nil || json.Unmarshal([]byte(raw), &tok) != nil || tok.SrcBucket != srcBucket || tok.SrcName != srcName {
			apierr.Write(w, errInvalid("The rewrite token is invalid."))
			return
		}
		q.Set("sourceGeneration", strconv.FormatInt(tok.SrcGen, 10))
	}
	src, err := s.copySource(ctx, q, srcBucket, srcName)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	size := int64(src.Object.Size)
	chunk := defaultRewriteChunk
	if v := q.Get("maxBytesRewrittenPerCall"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			chunk = max(n, 1<<20)
		}
	}
	tok.SrcBucket, tok.SrcName, tok.SrcGen = srcBucket, srcName, src.Object.Generation
	tok.Done += chunk
	resp := &storage.RewriteResponse{Kind: "storage#rewriteResponse", ObjectSize: size}
	resp.ForceSendFields = []string{"Done", "ObjectSize", "TotalBytesRewritten"}
	if tok.Done < size {
		b, _ := json.Marshal(tok)
		resp.TotalBytesRewritten = tok.Done
		resp.RewriteToken = encodeToken(string(b))
		writeJSON(w, http.StatusOK, resp)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	meta, err := copyMeta(src.Object, body, dstName)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, bkt, err := s.copyTo(ctx, src, dstBucket, dstName, meta, c, baseURL(r))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	resp.Done = true
	resp.TotalBytesRewritten = size
	resp.Resource = renderObject(rec, bkt, baseURL(r))
	writeJSON(w, http.StatusOK, resp)
}
