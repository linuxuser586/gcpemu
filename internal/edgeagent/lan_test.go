package edgeagent

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestControlLineRoundTrip(t *testing.T) {
	var b strings.Builder
	writeControl(&b, "127.0.0.1:40123")
	logs := "starting\n" + b.String() + "gcpemu agent lb-lan: listening on 10.0.0.5:80\n"
	if got, ok := readControl(logs); !ok || got != "127.0.0.1:40123" {
		t.Errorf("readControl = %q, %v; want 127.0.0.1:40123, true", got, ok)
	}
}

func TestControlLineMissing(t *testing.T) {
	for _, logs := range []string{"", "starting\nlisten tcp 127.0.0.1:0: bind: address in use\n"} {
		if got, ok := readControl(logs); ok {
			t.Errorf("readControl(%q) = %q, true; want not found", logs, got)
		}
	}
}

func TestLANAgentReportsControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveLAN(ctx, pw, "secret"); pw.Close() }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serveLAN: %v", err)
		}
	})
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, pr)
	addr, ok := readControl(line)
	if !ok {
		t.Fatalf("no control line in %q", line)
	}
	resp, err := http.Post("http://"+addr+"/config", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /config without token = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}
