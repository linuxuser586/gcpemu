// Command wiapp is the workload of the GKE real-hostname test (FR-INT-007).
// It uses the Cloud Storage and Pub/Sub Go clients exactly as an
// application on GKE would: default endpoints (storage.googleapis.com,
// pubsub.googleapis.com:443), default credentials (the metadata server via
// Workload Identity) and the system trust store. It is built with
// CGO_ENABLED=0 into an image without any CA certificates, so TLS only
// works if the emulator injected its CA bundle into the pod.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
)

func main() {
	project, bucket, topic := os.Getenv("PROJECT"), os.Getenv("BUCKET"), os.Getenv("TOPIC")
	var err error
	for i := 0; i < 30; i++ {
		if err = run(project, bucket, topic); err == nil {
			fmt.Println("WIAPP OK")
			return
		}
		fmt.Println("WIAPP RETRY:", err)
		if i == 0 {
			diagnose()
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("WIAPP FAILED:", err)
	os.Exit(1)
}

func run(project, bucket, topic string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sc, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("storage client: %w", err)
	}
	defer sc.Close()
	w := sc.Bucket(bucket).Object("from-pod.txt").NewWriter(ctx)
	if _, err := io.WriteString(w, "hello from a GKE pod"); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("storage write: %w", err)
	}
	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return fmt.Errorf("pubsub client: %w", err)
	}
	defer pc.Close()
	p := pc.Publisher(topic)
	defer p.Stop()
	if _, err := p.Publish(ctx, &pubsub.Message{Data: []byte("hello from a GKE pod")}).Get(ctx); err != nil {
		return fmt.Errorf("pubsub publish: %w", err)
	}
	return nil
}

// diagnose prints what the client libraries see of the environment.
func diagnose() {
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), "metadata.google.internal.")
	fmt.Println("DIAG lookup metadata.google.internal:", addrs, err)
	resp, err := http.Get("http://169.254.169.254")
	if err == nil {
		fmt.Println("DIAG GET 169.254.169.254:", resp.Status, resp.Header.Get("Metadata-Flavor"))
		resp.Body.Close()
	} else {
		fmt.Println("DIAG GET 169.254.169.254:", err)
	}
	addrs, err = net.DefaultResolver.LookupHost(context.Background(), "storage.googleapis.com")
	fmt.Println("DIAG lookup storage.googleapis.com:", addrs, err)
}
