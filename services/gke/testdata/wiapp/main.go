// Command wiapp is the workload of the GKE real-hostname test (FR-INT-007).
// It uses the Cloud Storage and Pub/Sub Go clients exactly as an
// application on GKE would: default endpoints (storage.googleapis.com,
// pubsub.googleapis.com:443), default credentials (the metadata server via
// Workload Identity) and the system trust store. It is built with
// CGO_ENABLED=0 into an image without any CA certificates, so TLS only
// works if the emulator injected its CA bundle into the pod.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/compute/metadata"
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	if err := bigObject(ctx, sc.Bucket(bucket).Object("big.bin")); err != nil {
		return err
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
	return allowedLocations(ctx)
}

// bigSize spans two chunks of the Writer's default 16 MiB, so the upload
// is resumable and the client follows the session URI the service
// returned, which must be https (#440).
const bigSize = 20 << 20

// bigObject uploads bigSize random bytes and reads them back.
func bigObject(ctx context.Context, o *storage.ObjectHandle) error {
	data := make([]byte, bigSize)
	r := rand.New(rand.NewPCG(440, 440))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	w := o.NewWriter(ctx)
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("big write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("big write: %w", err)
	}
	rd, err := o.NewReader(ctx)
	if err != nil {
		return fmt.Errorf("big read: %w", err)
	}
	defer rd.Close()
	got, err := io.ReadAll(rd)
	if err != nil {
		return fmt.Errorf("big read: %w", err)
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("big read: %d bytes differ from the %d written", len(got), len(data))
	}
	return nil
}

// allowedLocations checks that the metadata server keeps a pod in Google's
// universe, and that the Regional Access Boundary lookup client libraries
// send for it there reaches the emulator (#244).
func allowedLocations(ctx context.Context) error {
	if ud, err := metadata.GetWithContext(ctx, "universe/universe-domain"); err != nil || ud != "googleapis.com" {
		return fmt.Errorf("universe domain %q: %v", ud, err)
	}
	email, err := metadata.EmailWithContext(ctx, "default")
	if err != nil {
		return err
	}
	raw, err := metadata.GetWithContext(ctx, "instance/service-accounts/default/token")
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/"+email+"/allowedLocations", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct{ EncodedLocations string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 || out.EncodedLocations != "0x0" {
		return fmt.Errorf("allowedLocations: %s %+v %v", resp.Status, out, err)
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
