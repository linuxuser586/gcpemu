package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunServesOnPortUntilCancelled(t *testing.T) {
	env := map[string]string{"PORT": "0", "ECHO_HEADERS": "x-test"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func(k string) string { return env[k] }, func(a net.Addr) { addrs <- a })
	}()

	var addr net.Addr
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run didn't start listening")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr.String()+"/status/418", nil)
	req.Header.Set("X-Test", "hi")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := `{"method":"GET","path":"/status/418","headers":{"x-test":"hi"}}` + "\n"
	if resp.StatusCode != http.StatusTeapot || string(body) != want {
		t.Fatalf("got %d %s, want 418 %s", resp.StatusCode, body, want)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run didn't return after cancel")
	}
}
