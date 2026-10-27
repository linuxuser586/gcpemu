package gcs_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// do sends a raw request and returns status, headers and body.
func do(t *testing.T, method, url string, body io.Reader, hdr map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

type errEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		} `json:"errors"`
	} `json:"error"`
}

func envelope(t *testing.T, b []byte) errEnvelope {
	t.Helper()
	var e errEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("not a JSON error envelope: %s", b)
	}
	return e
}

func TestRawErrorsAndChecksums(t *testing.T) {
	inst := start(t)
	base := inst.GatewayURL()
	code, _, b := do(t, "GET", base+"/storage/v1/b/nope", nil, nil)
	e := envelope(t, b)
	if code != 404 || e.Error.Code != 404 || e.Error.Message != "The specified bucket does not exist." || e.Error.Errors[0].Reason != "notFound" {
		t.Errorf("missing bucket = %d %s", code, b)
	}
	code, _, b = do(t, "POST", base+"/storage/v1/b?project="+testProject, strings.NewReader(`{"name":"raw-bucket"}`), map[string]string{"Content-Type": "application/json"})
	if code != 200 {
		t.Fatalf("create = %d %s", code, b)
	}
	var bucket map[string]any
	_ = json.Unmarshal(b, &bucket)
	if bucket["kind"] != "storage#bucket" || bucket["location"] != "US" || bucket["etag"] != "CAE=" || bucket["metageneration"] != "1" {
		t.Errorf("bucket = %s", b)
	}
	code, _, b = do(t, "POST", base+"/storage/v1/b", strings.NewReader(`{"name":"x-bucket"}`), nil)
	if e := envelope(t, b); code != 400 || e.Error.Errors[0].Reason != "required" {
		t.Errorf("missing project = %d %s", code, b)
	}

	// Simple upload with a correct and an incorrect MD5 (x-goog-hash).
	data := []byte("hello world")
	sum := md5.Sum(data)
	good := base64.StdEncoding.EncodeToString(sum[:])
	up := base + "/upload/storage/v1/b/raw-bucket/o?uploadType=media&name=a%2Fb.txt"
	code, _, b = do(t, "POST", up, bytes.NewReader(data), map[string]string{"Content-Type": "text/plain", "X-Goog-Hash": "md5=" + good})
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	if code != 200 || obj["md5Hash"] != good || obj["size"] != "11" || obj["crc32c"] != "yZRlqg==" || obj["contentType"] != "text/plain" {
		t.Errorf("upload = %d %s", code, b)
	}
	code, _, b = do(t, "POST", up, bytes.NewReader(data), map[string]string{"X-Goog-Hash": "md5=AAAAAAAAAAAAAAAAAAAAAA=="})
	if e := envelope(t, b); code != 400 || !strings.Contains(e.Error.Message, "MD5") || e.Error.Errors[0].Reason != "invalid" {
		t.Errorf("md5 mismatch = %d %s", code, b)
	}
	// Multipart with a wrong crc32c in the metadata.
	body := "--BOUND\r\nContent-Type: application/json\r\n\r\n{\"name\":\"m.txt\",\"crc32c\":\"AAAAAA==\"}\r\n--BOUND\r\nContent-Type: text/plain\r\n\r\nhello world\r\n--BOUND--\r\n"
	code, _, b = do(t, "POST", base+"/upload/storage/v1/b/raw-bucket/o?uploadType=multipart", strings.NewReader(body), map[string]string{"Content-Type": "multipart/related; boundary=BOUND"})
	if e := envelope(t, b); code != 400 || !strings.Contains(e.Error.Message, "CRC32C") {
		t.Errorf("crc mismatch = %d %s", code, b)
	}
	code, _, _ = do(t, "GET", base+"/storage/v1/b/raw-bucket/o/m.txt", nil, nil)
	if code != 404 {
		t.Errorf("object after failed upload = %d", code)
	}
	// Preconditions: 412 conditionNotMet; 304 on read NotMatch.
	code, _, b = do(t, "POST", up+"&ifGenerationMatch=0", bytes.NewReader(data), nil)
	if e := envelope(t, b); code != 412 || e.Error.Errors[0].Reason != "conditionNotMet" {
		t.Errorf("precondition = %d %s", code, b)
	}
	code, _, _ = do(t, "GET", fmt.Sprintf("%s/storage/v1/b/raw-bucket/o/a%%2Fb.txt?ifGenerationNotMatch=%v", base, obj["generation"]), nil, nil)
	if code != 304 {
		t.Errorf("not modified = %d", code)
	}
	// alt=media with Range.
	code, h, b := do(t, "GET", base+"/download/storage/v1/b/raw-bucket/o/a%2Fb.txt?alt=media", nil, map[string]string{"Range": "bytes=6-"})
	if code != 206 || string(b) != "world" || h.Get("Content-Range") != "bytes 6-10/11" || h.Get("X-Goog-Generation") == "" {
		t.Errorf("range = %d %q %v", code, b, h)
	}
	code, _, _ = do(t, "GET", base+"/storage/v1/b/raw-bucket/o/a%2Fb.txt?alt=media", nil, map[string]string{"Range": "bytes=50-"})
	if code != 416 {
		t.Errorf("unsatisfiable range = %d", code)
	}
	code, _, b = do(t, "DELETE", base+"/storage/v1/b/raw-bucket", nil, nil)
	if e := envelope(t, b); code != 409 || e.Error.Errors[0].Reason != "conflict" {
		t.Errorf("non-empty delete = %d %s", code, b)
	}
}

// TestResumableOpenEnded covers "bytes O-*/T", which the Node client
// sends for a single-request upload: the body runs from O to its end, and
// the upload completes there when T is * or the object reaches T (#601).
func TestResumableOpenEnded(t *testing.T) {
	inst := start(t)
	base := "http://" + inst.Endpoint("gcs")
	do(t, "POST", base+"/storage/v1/b?project="+testProject, strings.NewReader(`{"name":"open"}`), nil)
	session := func(name string) string {
		t.Helper()
		code, h, b := do(t, "POST", base+"/upload/storage/v1/b/open/o?uploadType=resumable&name="+name, nil, nil)
		if code != 200 || h.Get("Location") == "" {
			t.Fatalf("init %s = %d %s", name, code, b)
		}
		return h.Get("Location")
	}
	data := []byte(strings.Repeat("0123456789", 1000)) // 10000 bytes
	type put struct {
		body         []byte
		contentRange string
		want         int
	}
	for _, c := range []struct {
		name string
		puts []put
	}{
		{"whole.bin", []put{{data, "bytes 0-*/*", 200}}},
		{"rest.bin", []put{{data[:4000], "bytes 0-3999/*", 308}, {data[4000:], "bytes 4000-*/10000", 200}}},
		// Bytes past the total are not the object's.
		{"capped.bin", []put{{append(slices.Clone(data), "extra"...), "bytes 0-*/10000", 200}}},
	} {
		loc := session(c.name)
		for _, p := range c.puts {
			code, _, b := do(t, "PUT", loc, bytes.NewReader(p.body), map[string]string{"Content-Range": p.contentRange})
			if code != p.want {
				t.Fatalf("%s: PUT %q = %d %s, want %d", c.name, p.contentRange, code, b, p.want)
			}
		}
		if code, _, got := do(t, "GET", base+"/open/"+c.name, nil, nil); code != 200 || !bytes.Equal(got, data) {
			t.Errorf("%s: read = %d, %d bytes", c.name, code, len(got))
		}
	}
}

func TestResumableChunkResume(t *testing.T) {
	inst := start(t)
	base := "http://" + inst.Endpoint("gcs")
	do(t, "POST", base+"/storage/v1/b?project="+testProject, strings.NewReader(`{"name":"resume"}`), nil)
	code, h, b := do(t, "POST", base+"/upload/storage/v1/b/resume/o?uploadType=resumable&name=big.bin",
		strings.NewReader(`{"contentType":"application/x-test","metadata":{"k":"v"}}`), map[string]string{"Content-Type": "application/json"})
	loc := h.Get("Location")
	if code != 200 || !strings.Contains(loc, "upload_id=") {
		t.Fatalf("init = %d %s %v", code, b, h)
	}
	data := []byte(strings.Repeat("0123456789", 1000)) // 10000 bytes
	// First chunk: 4000 bytes.
	code, h, _ = do(t, "PUT", loc, bytes.NewReader(data[:4000]), map[string]string{"Content-Range": "bytes 0-3999/*"})
	if code != 308 || h.Get("Range") != "bytes=0-3999" {
		t.Fatalf("chunk1 = %d %v", code, h)
	}
	// Status query.
	code, h, _ = do(t, "PUT", loc, nil, map[string]string{"Content-Range": "bytes */*"})
	if code != 308 || h.Get("Range") != "bytes=0-3999" {
		t.Fatalf("status = %d %v", code, h)
	}
	// Client resends an overlapping chunk (as after a lost response), with
	// the no-308 header.
	code, h, _ = do(t, "PUT", loc, bytes.NewReader(data[2000:8000]), map[string]string{"Content-Range": "bytes 2000-7999/*", "X-GUploader-No-308": "yes"})
	if code != 200 || h.Get("X-Http-Status-Code-Override") != "308" || h.Get("Range") != "bytes=0-7999" {
		t.Fatalf("overlap = %d %v", code, h)
	}
	// Final chunk with CRC32C over the whole object.
	code, _, b = do(t, "PUT", loc, bytes.NewReader(data[8000:]), map[string]string{"Content-Range": "bytes 8000-9999/10000"})
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	if code != 200 || obj["size"] != "10000" || obj["contentType"] != "application/x-test" {
		t.Fatalf("final = %d %s", code, b)
	}
	// Retrying the final request is idempotent.
	code, _, b2 := do(t, "PUT", loc, nil, map[string]string{"Content-Range": "bytes */10000"})
	var obj2 map[string]any
	_ = json.Unmarshal(b2, &obj2)
	if code != 200 || obj2["generation"] != obj["generation"] {
		t.Errorf("retry final = %d %s", code, b2)
	}
	code, _, got := do(t, "GET", base+"/resume/big.bin", nil, nil)
	if code != 200 || !bytes.Equal(got, data) {
		t.Errorf("xml read = %d len %d", code, len(got))
	}
	// Cancel a session.
	_, h, _ = do(t, "POST", base+"/upload/storage/v1/b/resume/o?uploadType=resumable&name=x", nil, nil)
	if code, _, _ := do(t, "DELETE", h.Get("Location"), nil, nil); code != 499 {
		t.Errorf("cancel = %d", code)
	}
}

func TestXMLAPI(t *testing.T) {
	inst := start(t)
	base := "http://" + inst.Endpoint("gcs")
	code, _, b := do(t, "PUT", base+"/xml-bucket", nil, map[string]string{"X-Goog-Project-Id": testProject})
	if code != 200 {
		t.Fatalf("create = %d %s", code, b)
	}
	code, h, _ := do(t, "PUT", base+"/xml-bucket/dir/one.txt", strings.NewReader("one"), map[string]string{"Content-Type": "text/plain", "X-Goog-Meta-Color": "red"})
	if code != 200 || h.Get("X-Goog-Generation") == "" || h.Get("ETag") == "" {
		t.Fatalf("put = %d %v", code, h)
	}
	code, h, b = do(t, "GET", base+"/xml-bucket/dir/one.txt", nil, nil)
	if code != 200 || string(b) != "one" || h.Get("X-Goog-Meta-Color") != "red" || h.Get("Content-Type") != "text/plain" {
		t.Errorf("get = %d %q %v", code, b, h)
	}
	code, _, b = do(t, "GET", base+"/xml-bucket/missing", nil, nil)
	if code != 404 || !strings.Contains(string(b), "<Code>NoSuchKey</Code>") {
		t.Errorf("missing = %d %s", code, b)
	}
	// Multipart upload.
	code, _, b = do(t, "POST", base+"/xml-bucket/mpu.bin?uploads", nil, nil)
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(b, &init); err != nil || code != 200 || init.UploadID == "" {
		t.Fatalf("initiate = %d %s", code, b)
	}
	var etags []string
	for i, part := range []string{"part-one|", "part-two"} {
		code, h, _ := do(t, "PUT", fmt.Sprintf("%s/xml-bucket/mpu.bin?partNumber=%d&uploadId=%s", base, i+1, init.UploadID), strings.NewReader(part), nil)
		if code != 200 {
			t.Fatalf("part %d = %d", i, code)
		}
		etags = append(etags, h.Get("ETag"))
	}
	complete := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", etags[0], etags[1])
	code, _, b = do(t, "POST", base+"/xml-bucket/mpu.bin?uploadId="+init.UploadID, strings.NewReader(complete), nil)
	if code != 200 || !strings.Contains(string(b), "CompleteMultipartUploadResult") {
		t.Fatalf("complete = %d %s", code, b)
	}
	if _, _, b := do(t, "GET", base+"/xml-bucket/mpu.bin", nil, nil); string(b) != "part-one|part-two" {
		t.Errorf("mpu content = %q", b)
	}
	// Listing with a delimiter.
	code, _, b = do(t, "GET", base+"/xml-bucket?delimiter=/", nil, nil)
	var lr struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
		CommonPrefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
	}
	if err := xml.Unmarshal(b, &lr); err != nil || code != 200 || len(lr.Contents) != 1 || lr.Contents[0].Key != "mpu.bin" || len(lr.CommonPrefixes) != 1 || lr.CommonPrefixes[0].Prefix != "dir/" {
		t.Errorf("list = %d %s", code, b)
	}
	if code, _, _ := do(t, "DELETE", base+"/xml-bucket/dir/one.txt", nil, nil); code != 204 {
		t.Errorf("delete = %d", code)
	}
}

func TestBatch(t *testing.T) {
	inst := start(t)
	base := inst.GatewayURL()
	do(t, "POST", base+"/storage/v1/b?project="+testProject, strings.NewReader(`{"name":"batchb"}`), nil)
	for _, n := range []string{"a", "b"} {
		do(t, "POST", base+"/upload/storage/v1/b/batchb/o?uploadType=media&name="+n, strings.NewReader(n), nil)
	}
	body := "--B\r\nContent-Type: application/http\r\nContent-ID: <1>\r\n\r\nDELETE /storage/v1/b/batchb/o/a HTTP/1.1\r\n\r\n\r\n" +
		"--B\r\nContent-Type: application/http\r\nContent-ID: <2>\r\n\r\nGET /storage/v1/b/batchb/o/zzz HTTP/1.1\r\n\r\n\r\n--B--\r\n"
	code, h, b := do(t, "POST", base+"/batch/storage/v1", strings.NewReader(body), map[string]string{"Content-Type": "multipart/mixed; boundary=B"})
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "multipart/mixed") || !strings.Contains(string(b), "HTTP/1.1 204") || !strings.Contains(string(b), "HTTP/1.1 404") || !strings.Contains(string(b), "response-1") {
		t.Errorf("batch = %d %s", code, b)
	}
}

func TestXMLResponseOverrides(t *testing.T) {
	inst := start(t)
	base := "http://" + inst.Endpoint("gcs")
	if code, _, b := do(t, "PUT", base+"/overrides", nil, map[string]string{"X-Goog-Project-Id": testProject}); code != 200 {
		t.Fatalf("create = %d %s", code, b)
	}
	if code, _, b := do(t, "PUT", base+"/overrides/a.svg", strings.NewReader("<svg/>"), map[string]string{"Content-Type": "image/svg+xml", "Cache-Control": "no-store"}); code != 200 {
		t.Fatalf("put = %d %s", code, b)
	}
	q := url.Values{
		"response-content-type":        {"text/plain"},
		"response-content-disposition": {`attachment; filename="notes.txt"`},
		"response-cache-control":       {"private, max-age=60"},
		"response-content-language":    {"en"},
		"response-expires":             {"Thu, 01 Dec 1994 16:00:00 GMT"},
	}
	want := map[string]string{
		"Content-Type":        "text/plain",
		"Content-Disposition": `attachment; filename="notes.txt"`,
		"Cache-Control":       "private, max-age=60",
		"Content-Language":    "en",
		"Expires":             "Thu, 01 Dec 1994 16:00:00 GMT",
	}
	for _, method := range []string{"GET", "HEAD"} {
		code, h, b := do(t, method, base+"/overrides/a.svg?"+q.Encode(), nil, nil)
		if code != 200 {
			t.Fatalf("%s = %d %s", method, code, b)
		}
		for k, v := range want {
			if h.Get(k) != v {
				t.Errorf("%s %s = %q, want %q", method, k, h.Get(k), v)
			}
		}
		if method == "GET" && string(b) != "<svg/>" {
			t.Errorf("GET body = %q", b)
		}
	}
	// Without overrides the stored metadata is served.
	code, h, _ := do(t, "GET", base+"/overrides/a.svg", nil, nil)
	if code != 200 || h.Get("Content-Type") != "image/svg+xml" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Disposition") != "" {
		t.Errorf("plain get = %d %v", code, h)
	}
}

// signV4 signs req with an HMAC key the way S3 clients do (AWS Signature
// Version 4, docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv.html),
// with prefix "AWS4" or "GOOG4". query signs it as a presigned URL instead
// of an Authorization header.
func signV4(req *http.Request, prefix, accessID, secret string, query bool) {
	lower := strings.ToLower(prefix)
	date := time.Now().UTC().Format("20060102T150405Z")
	scope := date[:8] + "/us-central1/" + map[string]string{"AWS4": "s3", "GOOG4": "storage"}[prefix] + "/" + lower + "_request"
	alg := prefix + "-HMAC-SHA256"
	hdr := map[string]string{"AWS4": "X-Amz-", "GOOG4": "X-Goog-"}[prefix]
	signed := "host"
	q := req.URL.Query()
	if query {
		q.Set(hdr+"Algorithm", alg)
		q.Set(hdr+"Credential", accessID+"/"+scope)
		q.Set(hdr+"Date", date)
		q.Set(hdr+"Expires", "600")
		q.Set(hdr+"SignedHeaders", signed)
	} else {
		req.Header.Set(hdr+"Date", date)
		req.Header.Set(hdr+"Content-Sha256", "UNSIGNED-PAYLOAD")
		signed = "host;" + strings.ToLower(hdr) + "content-sha256;" + strings.ToLower(hdr) + "date"
	}
	canon := []string{req.Method, req.URL.EscapedPath(), strings.ReplaceAll(q.Encode(), "+", "%20"), "host:" + req.URL.Host}
	if !query {
		canon = append(canon, strings.ToLower(hdr)+"content-sha256:UNSIGNED-PAYLOAD", strings.ToLower(hdr)+"date:"+date)
	}
	canon = append(canon, "", signed, "UNSIGNED-PAYLOAD")
	sum := sha256.Sum256([]byte(strings.Join(canon, "\n")))
	key := []byte(prefix + secret)
	for _, part := range strings.Split(scope, "/") {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(part))
		key = m.Sum(nil)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.Join([]string{alg, date, scope, hex.EncodeToString(sum[:])}, "\n")))
	sig := hex.EncodeToString(m.Sum(nil))
	if query {
		q.Set(hdr+"Signature", sig)
		req.URL.RawQuery = q.Encode()
		return
	}
	req.Header.Set("Authorization", alg+" Credential="+accessID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

// An S3 client signs with AWS4-HMAC-SHA256 and reads user metadata from
// x-amz-meta-* (the aws CLI's --metadata), so a read signed that way names
// it so; any other read keeps x-goog-meta-*. botocore keeps a key as the
// response spells it, so the key goes out as stored, not canonicalized.
func TestXMLMetadataHeadersFollowSigner(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	k, err := c.CreateHMACKey(context.Background(), testProject, "s3@test-project.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + inst.Endpoint("gcs")
	if code, _, b := do(t, "PUT", base+"/meta-bucket", nil, map[string]string{"X-Goog-Project-Id": testProject}); code != 200 {
		t.Fatalf("create = %d %s", code, b)
	}
	if code, _, b := do(t, "PUT", base+"/meta-bucket/o.txt", strings.NewReader("o"), map[string]string{"X-Amz-Meta-Color": "red"}); code != 200 {
		t.Fatalf("put = %d %s", code, b)
	}
	for _, tc := range []struct {
		name, prefix string // prefix "" sends the request unsigned
		query        bool
		want, absent string // the header line that carries the metadata, and the one that must not
	}{
		{"unsigned", "", false, "x-goog-meta-color: red", "x-amz-meta-"},
		{"GOOG4 header", "GOOG4", false, "x-goog-meta-color: red", "x-amz-meta-"},
		{"GOOG4 query", "GOOG4", true, "x-goog-meta-color: red", "x-amz-meta-"},
		{"AWS4 header", "AWS4", false, "x-amz-meta-color: red", "x-goog-meta-"},
		{"AWS4 query", "AWS4", true, "x-amz-meta-color: red", "x-goog-meta-"},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			req, _ := http.NewRequest(method, base+"/meta-bucket/o.txt", nil)
			if tc.prefix != "" {
				signV4(req, tc.prefix, k.AccessID, k.Secret, tc.query)
			}
			// The raw response: Go's client would canonicalize the names.
			req.Close = true // so the read below ends with the response
			conn, err := net.Dial("tcp", inst.Endpoint("gcs"))
			if err != nil {
				t.Fatal(err)
			}
			if err := req.Write(conn); err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(io.LimitReader(conn, 1<<16))
			conn.Close()
			head, _, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
			if !bytes.HasPrefix(head, []byte("HTTP/1.1 200 ")) || !bytes.Contains(head, []byte("\r\n"+tc.want+"\r\n")) || bytes.Contains(bytes.ToLower(head), []byte("\r\n"+tc.absent)) {
				t.Errorf("%s %s: want %q and no %s* in:\n%s", tc.name, method, tc.want, tc.absent, head)
			}
		}
	}
}

// gzipped returns b gzip-compressed, as google-api-client (the Java
// library) sends JSON and upload bodies.
func gzipped(t *testing.T, b []byte) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// Content-Encoding: gzip on a JSON API or upload request is transport
// encoding: the request means what its decoded body says (#602).
func TestRawGzipRequestBodies(t *testing.T) {
	inst := start(t)
	base := inst.GatewayURL()
	gz := map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip"}
	code, _, b := do(t, "POST", base+"/storage/v1/b?project="+testProject, gzipped(t, []byte(`{"name":"gzip-bucket"}`)), gz)
	if code != 200 {
		t.Fatalf("gzipped bucket insert = %d %s", code, b)
	}

	data := []byte{0, 1, 2, 0xfe, 0xff, '\n', '\r', 0}
	media := base + "/upload/storage/v1/b/gzip-bucket/o?uploadType=media&name=media.bin"
	code, _, b = do(t, "POST", media, gzipped(t, data), map[string]string{"Content-Type": "application/octet-stream", "Content-Encoding": "gzip"})
	if code != 200 {
		t.Fatalf("gzipped media upload = %d %s", code, b)
	}
	multipart := "--BOUND\r\nContent-Type: application/json\r\n\r\n{\"name\":\"multipart.bin\"}\r\n--BOUND\r\nContent-Type: application/octet-stream\r\n\r\n" + string(data) + "\r\n--BOUND--\r\n"
	code, _, b = do(t, "POST", base+"/upload/storage/v1/b/gzip-bucket/o?uploadType=multipart", gzipped(t, []byte(multipart)),
		map[string]string{"Content-Type": "multipart/related; boundary=BOUND", "Content-Encoding": "gzip"})
	if code != 200 {
		t.Fatalf("gzipped multipart upload = %d %s", code, b)
	}
	for _, name := range []string{"media.bin", "multipart.bin"} {
		code, _, b = do(t, "GET", base+"/storage/v1/b/gzip-bucket/o/"+name, nil, nil)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		if code != 200 || obj["size"] != "8" || obj["contentEncoding"] != nil {
			t.Errorf("%s metadata = %d %s, want size 8 and no contentEncoding", name, code, b)
		}
		code, _, b = do(t, "GET", base+"/download/storage/v1/b/gzip-bucket/o/"+name+"?alt=media", nil, nil)
		if code != 200 || !bytes.Equal(b, data) {
			t.Errorf("%s = %d %q, want %q", name, code, b, data)
		}
	}

	code, _, b = do(t, "POST", base+"/storage/v1/b?project="+testProject, strings.NewReader("\x1f\x8bnot gzip"), gz)
	if e := envelope(t, b); code != 400 || e.Error.Errors[0].Reason != "invalid" {
		t.Errorf("corrupt gzip body = %d %s", code, b)
	}

	// On the XML API the header is the object's Content-Encoding.
	code, _, b = do(t, "PUT", base+"/gzip-bucket/stored.gz", gzipped(t, data), map[string]string{"Content-Encoding": "gzip"})
	if code != 200 {
		t.Fatalf("XML PUT = %d %s", code, b)
	}
	code, _, b = do(t, "GET", base+"/storage/v1/b/gzip-bucket/o/stored.gz", nil, nil)
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	if code != 200 || obj["contentEncoding"] != "gzip" {
		t.Errorf("XML-uploaded object = %d %s, want contentEncoding gzip", code, b)
	}
}
