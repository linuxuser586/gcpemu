package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve sends req to a handler reporting the headers named in echoHeaders.
func serve(echoHeaders string, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	newHandler(echoHeaders).ServeHTTP(rec, req)
	return rec
}

func TestStatusFromPath(t *testing.T) {
	rec := serve("", httptest.NewRequest(http.MethodGet, "/status/503", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestReportsListedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/status/404?a=1&b=2", nil)
	req.Header.Set("X-Test-One", "one")
	req.Header.Add("Accept-Language", "en")
	req.Header.Add("Accept-Language", "fr")
	req.Header.Set("X-Not-Listed", "secret")
	rec := serve(" x-test-one, Accept-Language,COOKIE ,", req)

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	want := `{"method":"POST","path":"/status/404?a=1&b=2","headers":{"accept-language":"en, fr","cookie":null,"x-test-one":"one"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
}

func TestReportsHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	want := `{"method":"GET","path":"/","headers":{"host":"example.test"}}` + "\n"
	if got := serve("Host", req).Body.String(); got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
}

func TestIdenticalRequestsGetIdenticalBodies(t *testing.T) {
	const names = "a,b,c,d,e,f,g,h,i,j,k,l"
	body := func() string {
		req := httptest.NewRequest(http.MethodGet, "/error-page", nil)
		for _, n := range []string{"a", "c", "e", "g", "i", "k"} {
			req.Header.Set(n, "v"+n)
		}
		return serve(names, req).Body.String()
	}
	first := body()
	for range 20 {
		if got := body(); got != first {
			t.Fatalf("body = %s\nfirst  %s", got, first)
		}
	}
}

func TestBadStatusIsBadRequest(t *testing.T) {
	for _, path := range []string{"/status/", "/status/abc", "/status/700", "/status/199", "/status/+503", "/status/503/x"} {
		rec := serve("", httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestOtherPathIsOK(t *testing.T) {
	for _, path := range []string{"/", "/error-page", "/status", "/statuses/503"} {
		rec := serve("", httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}
