package gcs

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Signed POST policy uploads (FR-GCS-006): an HTML form POST to /BUCKET
// whose policy document is signed with a service account or HMAC key.

// postPolicy is the decoded policy document.
type postPolicy struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

func policyErr(details string) *sigError {
	return &sigError{http.StatusBadRequest, "InvalidPolicyDocument", "The content of the form does not meet the conditions specified in the policy document.", details}
}

func (s *Service) postPolicyUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	mt, params, err := parseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		xmlError(w, errInvalid("POST object requires multipart/form-data."))
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	fields := map[string]string{}
	var file *multipart.Part
	var filename string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			xmlError(w, errInvalid("Malformed form: %v", err))
			return
		}
		name := strings.ToLower(p.FormName())
		if name == "file" {
			file, filename = p, p.FileName()
			break // the file must be the last field
		}
		b, _ := io.ReadAll(io.LimitReader(p, 1<<20))
		fields[name] = string(b)
	}
	if file == nil {
		xmlError(w, errInvalid("POST requires a file field."))
		return
	}
	key := strings.ReplaceAll(fields["key"], "${filename}", filename)
	fields["key"] = key
	fields["bucket"] = bucket
	ctx := r.Context()
	if fields["policy"] != "" || fields["x-goog-signature"] != "" {
		pol, who, err := s.verifyPolicy(r, fields)
		if err != nil {
			xmlError(w, err)
			return
		}
		ctx = emu.WithPrincipal(ctx, who)
		lr := &countingReader{r: file}
		meta := policyMeta(fields, key)
		rec, _, err := s.putObject(ctx, bucket, meta, lr, "", url.Values{}, baseURL(r))
		if err != nil {
			xmlError(w, err)
			return
		}
		if err := checkLengthRange(pol, int64(rec.Object.Size)); err != nil {
			_ = s.removeObject(ctx, bucket, key, rec.Object.Generation, conds{}, baseURL(r))
			xmlError(w, err)
			return
		}
		postResponse(w, r, fields, rec.Object)
		return
	}
	rec, _, err := s.putObject(ctx, bucket, policyMeta(fields, key), file, "", url.Values{}, baseURL(r))
	if err != nil {
		xmlError(w, err)
		return
	}
	postResponse(w, r, fields, rec.Object)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// policyMeta builds object metadata from form fields.
func policyMeta(f map[string]string, key string) *storage.Object {
	m := &storage.Object{
		Name: key, ContentType: f["content-type"], CacheControl: f["cache-control"],
		ContentDisposition: f["content-disposition"], ContentEncoding: f["content-encoding"],
	}
	for k, v := range f {
		if strings.HasPrefix(k, "x-goog-meta-") {
			if m.Metadata == nil {
				m.Metadata = map[string]string{}
			}
			m.Metadata[strings.TrimPrefix(k, "x-goog-meta-")] = v
		}
	}
	return m
}

// verifyPolicy checks the policy signature, expiry and field conditions.
func (s *Service) verifyPolicy(r *http.Request, f map[string]string) (*postPolicy, emu.Principal, error) {
	b64 := f["policy"]
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, "", policyErr("The policy is not valid base64.")
	}
	var pol postPolicy
	if err := json.Unmarshal(raw, &pol); err != nil {
		return nil, "", policyErr("The policy is not valid JSON.")
	}
	alg := f["x-goog-algorithm"]
	cred := strings.Split(f["x-goog-credential"], "/")
	if alg == "" || len(cred) != 5 {
		return nil, "", errMalformed("Invalid x-goog-credential: " + f["x-goog-credential"])
	}
	sig, err := hex.DecodeString(f["x-goog-signature"])
	if err != nil {
		return nil, "", errSignature("The signature is not valid hex.")
	}
	who, err := s.verifySignature(r.Context(), alg, cred[0], cred, sig, []string{b64})
	if err != nil {
		return nil, "", err
	}
	exp, err := time.Parse(time.RFC3339, pol.Expiration)
	if err != nil {
		return nil, "", policyErr("Invalid expiration in policy.")
	}
	if s.now().After(exp) {
		return nil, "", errExpired("Invalid according to Policy: Policy expired.")
	}
	for _, c := range pol.Conditions {
		if err := checkCondition(c, f); err != nil {
			return nil, "", err
		}
	}
	return &pol, who, nil
}

// checkCondition evaluates one policy condition against the form fields.
// Content-length ranges are checked after the upload.
func checkCondition(c json.RawMessage, f map[string]string) error {
	var obj map[string]string
	if json.Unmarshal(c, &obj) == nil {
		for k, v := range obj {
			if f[strings.ToLower(k)] != v {
				return policyErr(fmt.Sprintf("Invalid according to Policy: Policy Condition failed: [\"eq\", \"$%s\", %q]", k, v))
			}
		}
		return nil
	}
	var arr []any
	if err := json.Unmarshal(c, &arr); err != nil || len(arr) != 3 {
		return policyErr("Invalid policy condition.")
	}
	op, _ := arr[0].(string)
	switch strings.ToLower(op) {
	case "content-length-range":
		return nil
	case "eq", "starts-with":
		field, _ := arr[1].(string)
		want, _ := arr[2].(string)
		got := f[strings.ToLower(strings.TrimPrefix(field, "$"))]
		if op == "eq" && got != want || strings.EqualFold(op, "starts-with") && !strings.HasPrefix(got, want) {
			return policyErr(fmt.Sprintf("Invalid according to Policy: Policy Condition failed: [%q, %q, %q]", op, field, want))
		}
		return nil
	}
	return policyErr("Unsupported policy condition: " + op)
}

func checkLengthRange(pol *postPolicy, size int64) error {
	for _, c := range pol.Conditions {
		var arr []any
		if json.Unmarshal(c, &arr) != nil || len(arr) != 3 {
			continue
		}
		if op, _ := arr[0].(string); strings.ToLower(op) != "content-length-range" {
			continue
		}
		lo, _ := arr[1].(float64)
		hi, _ := arr[2].(float64)
		if size < int64(lo) || size > int64(hi) {
			return &sigError{http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.",
				fmt.Sprintf("Content-length %d is outside the range [%d, %d].", size, int64(lo), int64(hi))}
		}
	}
	return nil
}

// postResponse answers a successful form POST per success_action_redirect
// / success_action_status.
func postResponse(w http.ResponseWriter, r *http.Request, f map[string]string, o *storage.Object) {
	setXMLWriteHeaders(w, o)
	if red := f["success_action_redirect"]; red != "" {
		u, err := url.Parse(red)
		if err == nil {
			q := u.Query()
			q.Set("bucket", o.Bucket)
			q.Set("key", o.Name)
			q.Set("etag", md5Hex(o))
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusSeeOther)
			return
		}
	}
	code, _ := strconv.Atoi(f["success_action_status"])
	switch code {
	case http.StatusOK:
		w.WriteHeader(http.StatusOK)
	case http.StatusCreated:
		type result struct {
			XMLName  xml.Name `xml:"PostResponse"`
			Location string   `xml:"Location"`
			Bucket   string   `xml:"Bucket"`
			Key      string   `xml:"Key"`
			ETag     string   `xml:"ETag"`
		}
		writeXML(w, http.StatusCreated, result{Location: baseURL(r) + "/" + o.Bucket + "/" + pathEncodeV4(o.Name), Bucket: o.Bucket, Key: o.Name, ETag: md5Hex(o)})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
