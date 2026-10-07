// Command app is the Go API of the SRS 11.2 reference stack. It runs on the
// emulated GKE cluster behind the load balancer and the Istio ingress
// gateway and uses Google Cloud exactly as a production service would:
// default endpoints, default credentials (the GKE metadata server through
// Workload Identity) and no emulator configuration at all.
//
//	GET  /api/health    liveness through LB → Istio (step 4)
//	POST /api/work      writes an object to Cloud Storage, inserts a row in
//	                    Cloud SQL through the Go connector with IAM database
//	                    authentication over private IP, publishes a Pub/Sub
//	                    message (step 5); 403 + PERMISSION_DENIED when IAM
//	                    denies any of them (step 8)
//	POST /api/push      Pub/Sub push endpoint; verifies the OIDC token
//	                    (step 6) and records the delivery
//	GET  /api/received  deliveries recorded by /api/push
//	GET  /api/egress    fetches ?url= from the internet (step 7)
//
// Configuration (environment): PROJECT, BUCKET, TOPIC, SQL_INSTANCE
// (connection name), SQL_USER (IAM user), SQL_DB, PUSH_AUDIENCE,
// PUSH_SA (expected token email), PORT (default 8080).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/idtoken"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type app struct {
	project, bucket, topic      string
	sqlInstance, sqlUser, sqlDB string
	pushAudience, pushSA        string
	host                        string
	gcs                         *storage.Client
	ps                          *pubsub.Client
	pub                         *pubsub.Publisher
	sql                         *cloudsqlconn.Dialer
	mu                          sync.Mutex
	received                    []delivery
}

// delivery is one push request as seen by /api/push.
type delivery struct {
	Time         time.Time         `json:"time"`
	Verified     bool              `json:"verified"`
	Error        string            `json:"error,omitempty"`
	Email        string            `json:"email,omitempty"`
	Audience     string            `json:"audience,omitempty"`
	Issuer       string            `json:"issuer,omitempty"`
	Subscription string            `json:"subscription,omitempty"`
	MessageID    string            `json:"messageId,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Data         string            `json:"data,omitempty"`
}

func main() {
	a := &app{
		project: os.Getenv("PROJECT"), bucket: os.Getenv("BUCKET"), topic: os.Getenv("TOPIC"),
		sqlInstance: os.Getenv("SQL_INSTANCE"), sqlUser: os.Getenv("SQL_USER"), sqlDB: os.Getenv("SQL_DB"),
		pushAudience: os.Getenv("PUSH_AUDIENCE"), pushSA: os.Getenv("PUSH_SA"),
	}
	a.host, _ = os.Hostname()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("POST /api/work", a.work)
	mux.HandleFunc("POST /api/push", a.push)
	mux.HandleFunc("GET /api/received", a.listReceived)
	mux.HandleFunc("GET /api/egress", a.egress)
	srv := &http.Server{Addr: ":" + port, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("app listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s (%v)", r.Method, r.URL.Path, r.Header.Get("X-Forwarded-For"), time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "pod": a.host, "proto": r.Proto,
		"forwardedProto": r.Header.Get("X-Forwarded-Proto"), "forwardedFor": r.Header.Get("X-Forwarded-For")})
}

// clients creates the Google clients on first use: default endpoints and
// Application Default Credentials only.
func (a *app) clients(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if a.gcs == nil {
		if a.gcs, err = storage.NewClient(ctx); err != nil {
			return fmt.Errorf("storage client: %w", err)
		}
	}
	if a.ps == nil {
		if a.ps, err = pubsub.NewClient(ctx, a.project); err != nil {
			return fmt.Errorf("pubsub client: %w", err)
		}
		a.pub = a.ps.Publisher(a.topic)
	}
	if a.sql == nil {
		a.sql, err = cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithIAMAuthN(),
			cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()))
		if err != nil {
			return fmt.Errorf("cloud sql dialer: %w", err)
		}
	}
	return nil
}

// work performs step 5: GCS write, Cloud SQL insert, Pub/Sub publish.
func (a *app) work(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := a.clients(ctx); err != nil {
		fail(w, "init", err)
		return
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	if v := r.URL.Query().Get("id"); v != "" {
		id = v
	}
	out := map[string]any{"id": id, "pod": a.host}

	object := "work/" + id + ".txt"
	ow := a.gcs.Bucket(a.bucket).Object(object).NewWriter(ctx)
	ow.ContentType = "text/plain"
	if _, err := io.WriteString(ow, "work item "+id+" from "+a.host); err != nil {
		_ = ow.Close()
		fail(w, "gcs", err)
		return
	}
	if err := ow.Close(); err != nil {
		fail(w, "gcs", err)
		return
	}
	out["object"] = "gs://" + a.bucket + "/" + object

	row, err := a.insert(ctx, id)
	if err != nil {
		fail(w, "sql", err)
		return
	}
	out["row"] = row

	msgID, err := a.pub.Publish(ctx, &pubsub.Message{Data: []byte("work " + id),
		Attributes: map[string]string{"source": "app", "id": id}}).Get(ctx)
	if err != nil {
		fail(w, "pubsub", err)
		return
	}
	out["messageId"] = msgID
	writeJSON(w, http.StatusOK, out)
}

// insert adds a row through the Cloud SQL Go connector (IAM database
// authentication, private IP) and returns its id.
func (a *app) insert(ctx context.Context, id string) (int64, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("user=%s dbname=%s sslmode=disable", a.sqlUser, a.sqlDB))
	if err != nil {
		return 0, err
	}
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return a.sql.Dial(ctx, a.sqlInstance)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	var row int64
	err = conn.QueryRow(ctx, "INSERT INTO work_items (item, pod) VALUES ($1, $2) RETURNING id", id, a.host).Scan(&row)
	return row, err
}

// fail reports an error; denials map to 403 PERMISSION_DENIED.
func fail(w http.ResponseWriter, step string, err error) {
	code, reason := http.StatusBadGateway, "ERROR"
	var ge *googleapi.Error
	switch {
	case errors.As(err, &ge) && ge.Code == http.StatusForbidden,
		status.Code(err) == codes.PermissionDenied,
		strings.Contains(err.Error(), "PERMISSION_DENIED"):
		code, reason = http.StatusForbidden, "PERMISSION_DENIED"
	}
	log.Printf("%s failed: %v", step, err)
	writeJSON(w, code, map[string]string{"step": step, "code": reason, "error": err.Error()})
}

// push receives Pub/Sub push deliveries (wrapped payload) and verifies
// the OIDC token Pub/Sub attaches (step 6).
func (a *app) push(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message struct {
			Data       []byte            `json:"data"`
			Attributes map[string]string `json:"attributes"`
			MessageID  string            `json:"messageId"`
		} `json:"message"`
		Subscription string `json:"subscription"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d := delivery{Time: time.Now(), Subscription: body.Subscription, MessageID: body.Message.MessageID,
		Attributes: body.Message.Attributes, Data: string(body.Message.Data)}
	err := a.verify(r, &d)
	if err != nil {
		d.Error = err.Error()
	}
	d.Verified = err == nil
	a.mu.Lock()
	a.received = append(a.received, d)
	a.mu.Unlock()
	if err != nil {
		log.Printf("push rejected: %v", err)
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verify checks the bearer token's signature (Google's public keys at
// www.googleapis.com), audience, issuer and service account.
func (a *app) verify(r *http.Request, d *delivery) error {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errors.New("no bearer token")
	}
	p, err := idtoken.Validate(r.Context(), tok, a.pushAudience)
	if err != nil {
		return err
	}
	d.Audience, d.Issuer = p.Audience, p.Issuer
	d.Email, _ = p.Claims["email"].(string)
	if v, _ := p.Claims["email_verified"].(bool); !v {
		return errors.New("email not verified")
	}
	if a.pushSA != "" && d.Email != a.pushSA {
		return fmt.Errorf("token for %s, want %s", d.Email, a.pushSA)
	}
	return nil
}

func (a *app) listReceived(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	out := append([]delivery{}, a.received...)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// egress fetches an internet URL (step 7: works only through Cloud NAT).
func (a *app) egress(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": resp.StatusCode})
}
