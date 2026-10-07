package gcs_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
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
