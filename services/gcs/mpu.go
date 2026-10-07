package gcs

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// XML API multipart uploads (FR-GCS-005).

type mpuPart struct {
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

// mpuRec is a persisted XML multipart upload.
type mpuRec struct {
	ID      string           `json:"id"`
	Bucket  string           `json:"bucket"`
	Name    string           `json:"name"`
	Meta    *storage.Object  `json:"meta"`
	Created string           `json:"created"`
	Parts   map[int]*mpuPart `json:"parts"`
}

func (s *Service) partPath(id string, n int) string {
	return filepath.Join(s.blobs.uploadsDir(), fmt.Sprintf("mpu-%s-%05d", id, n))
}

func (s *Service) loadMPU(id, bucket, name string) (*mpuRec, error) {
	var m mpuRec
	err := s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsMPU, id, &m) })
	if err != nil || m.Bucket != bucket || m.Name != name {
		return nil, apierr.NotFound("The specified multipart upload does not exist.")
	}
	return &m, nil
}

func (s *Service) mpuInitiate(w http.ResponseWriter, r *http.Request, bucket, object string) {
	ctx := r.Context()
	if err := s.checkCreate(ctx, bucket, object); err != nil {
		xmlError(w, err)
		return
	}
	if _, err := s.getBucketRec(bucket); err != nil {
		xmlError(w, err)
		return
	}
	m := &mpuRec{ID: newUploadID(), Bucket: bucket, Name: object, Meta: xmlObjectMeta(r.Header, object), Created: ts(s.now()), Parts: map[int]*mpuPart{}}
	m.Meta.Md5Hash = ""
	if err := s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsMPU, m.ID, m) }); err != nil {
		xmlError(w, err)
		return
	}
	type result struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		XMLNS    string   `xml:"xmlns,attr"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}
	writeXML(w, http.StatusOK, result{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/", Bucket: bucket, Key: object, UploadID: m.ID})
}

func (s *Service) mpuServe(w http.ResponseWriter, r *http.Request, bucket, object, id string) {
	m, err := s.loadMPU(id, bucket, object)
	if err != nil {
		xmlError(w, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.mpuPutPart(w, r, m)
	case http.MethodPost:
		s.mpuComplete(w, r, m)
	case http.MethodDelete:
		s.abortMultipart(id)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		s.mpuListParts(w, m)
	default:
		xmlError(w, errInvalid("Method not allowed."))
	}
}

func (s *Service) mpuPutPart(w http.ResponseWriter, r *http.Request, m *mpuRec) {
	n, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || n < 1 || n > 10000 {
		xmlError(w, errInvalid("Part number must be an integer between 1 and 10000."))
		return
	}
	bw, err := s.blobs.newWriter()
	if err != nil {
		xmlError(w, err)
		return
	}
	if _, err := bw.ReadFrom(r.Body); err != nil {
		bw.abort()
		xmlError(w, errInvalid("Failed to read part: %v", err))
		return
	}
	if err := s.blobs.sync(bw.f); err != nil {
		bw.abort()
		xmlError(w, err)
		return
	}
	_ = bw.f.Close()
	sum := bw.md5.Sum(nil)
	if want := r.Header.Get("Content-Md5"); want != "" && want != (&blobInfo{MD5: sum}).md5B64() {
		_ = os.Remove(bw.f.Name())
		xmlError(w, errInvalid("The Content-MD5 you specified did not match what we received."))
		return
	}
	if err := os.Rename(bw.f.Name(), s.partPath(m.ID, n)); err != nil {
		_ = os.Remove(bw.f.Name())
		xmlError(w, err)
		return
	}
	etag := `"` + hex.EncodeToString(sum) + `"`
	err = s.env.Store.Update(func(tx store.Tx) error {
		var cur mpuRec
		if err := store.GetJSON(tx, nsMPU, m.ID, &cur); err != nil {
			return apierr.NotFound("The specified multipart upload does not exist.")
		}
		if cur.Parts == nil {
			cur.Parts = map[int]*mpuPart{}
		}
		cur.Parts[n] = &mpuPart{ETag: etag, Size: bw.n}
		return store.PutJSON(tx, nsMPU, m.ID, &cur)
	})
	if err != nil {
		xmlError(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Service) mpuListParts(w http.ResponseWriter, m *mpuRec) {
	type part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
		Size       int64  `xml:"Size"`
	}
	type result struct {
		XMLName  xml.Name `xml:"ListPartsResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
		Parts    []part   `xml:"Part"`
	}
	out := result{Bucket: m.Bucket, Key: m.Name, UploadID: m.ID}
	for n, p := range m.Parts {
		out.Parts = append(out.Parts, part{n, p.ETag, p.Size})
	}
	sort.Slice(out.Parts, func(i, j int) bool { return out.Parts[i].PartNumber < out.Parts[j].PartNumber })
	writeXML(w, http.StatusOK, out)
}

func (s *Service) mpuComplete(w http.ResponseWriter, r *http.Request, m *mpuRec) {
	var req struct {
		Parts []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		xmlError(w, errInvalid("The XML you provided was not well-formed or did not validate against our published schema."))
		return
	}
	bw, err := s.blobs.newWriter()
	if err != nil {
		xmlError(w, err)
		return
	}
	etags := md5.New()
	prev := 0
	for _, p := range req.Parts {
		part, ok := m.Parts[p.PartNumber]
		if !ok || p.PartNumber <= prev || (p.ETag != "" && strings.Trim(p.ETag, `"`) != strings.Trim(part.ETag, `"`)) {
			bw.abort()
			xmlError(w, errInvalid("One or more of the specified parts could not be found or the part order is invalid."))
			return
		}
		prev = p.PartNumber
		raw, _ := hex.DecodeString(strings.Trim(part.ETag, `"`))
		etags.Write(raw)
		f, err := os.Open(s.partPath(m.ID, p.PartNumber))
		if err != nil {
			bw.abort()
			xmlError(w, apierr.Internal("part data unavailable: %v", err))
			return
		}
		_, err = bw.ReadFrom(f)
		f.Close()
		if err != nil {
			bw.abort()
			xmlError(w, err)
			return
		}
	}
	info, err := bw.commit()
	if err != nil {
		xmlError(w, err)
		return
	}
	meta := *m.Meta
	rec, _, err := s.commitObject(r.Context(), &writeReq{bucket: m.Bucket, meta: &meta, blob: info, base: baseURL(r)})
	if err != nil {
		xmlError(w, err)
		return
	}
	s.abortMultipart(m.ID)
	type result struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		XMLNS    string   `xml:"xmlns,attr"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}
	setXMLWriteHeaders(w, rec.Object)
	etag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(etags.Sum(nil)), len(req.Parts))
	writeXML(w, http.StatusOK, result{
		XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/", Location: baseURL(r) + "/" + m.Bucket + "/" + pathEncodeV4(m.Name),
		Bucket: m.Bucket, Key: m.Name, ETag: etag,
	})
}

// abortMultipart deletes an XML multipart upload and its part files.
func (s *Service) abortMultipart(id string) {
	var m mpuRec
	_ = s.env.Store.Update(func(tx store.Tx) error {
		if store.GetJSON(tx, nsMPU, id, &m) != nil {
			return nil
		}
		return tx.Delete(nsMPU, id)
	})
	for n := range m.Parts {
		_ = os.Remove(s.partPath(id, n))
	}
}
