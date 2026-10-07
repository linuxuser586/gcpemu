// Package proxy is the Cloud SQL in-container agent (FR-SQL-004..007). It
// is the entrypoint of every instance container: it starts PostgreSQL (the
// image's docker-entrypoint.sh, listening on 127.0.0.1:5433 and a unix
// socket) as a child process, waits until it accepts TCP connections, and
// then serves
//
//   - :5432, a PostgreSQL-protocol-aware proxy that terminates TLS per the
//     instance's SSL mode, asks the emulator whether the connection is
//     allowed (authorized networks, SSL mode, connector enforcement), and
//     either passes BUILT_IN users through to PostgreSQL (SCRAM over
//     127.0.0.1) or performs IAM database authentication and logs the user
//     in over the unix socket (trust);
//   - :3307, the Cloud SQL connector / Auth Proxy server side: TLS 1.3 with
//     the instance's server certificate and a required client certificate
//     signed by the instance's client CA, followed by the same logic.
//
// Decisions are made by the emulator through a small HTTP endpoint (see
// LoginRequest) so that changes to authorized networks, users and IAM
// policies apply immediately.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Name is the agent name registered with internal/agent.
const Name = "sql-proxy"

// Environment variables and files the emulator provides to the agent.
const (
	EnvControl = "GCPEMU_SQL_CONTROL" // base URL of the emulator's agent endpoint
	EnvKey     = "GCPEMU_SQL_KEY"     // per-instance shared secret
	KeyHeader  = "X-Gcpemu-Agent-Key"

	Dir          = "/gcpemu"
	ServerCert   = "server.crt" // server certificate chain (PEM)
	ServerKey    = "server.key"
	ClientCA     = "client-ca.crt"
	HBAFile      = "pg_hba.conf"
	SocketDir    = "/var/run/postgresql"
	InternalPort = 5433
	PublicPort   = 5432
	ConnectPort  = 3307
)

// Listener names used in LoginRequest.
const (
	ListenerDirect    = "direct"    // :5432
	ListenerConnector = "connector" // :3307
)

// LoginRequest is sent by the agent for every client startup message.
type LoginRequest struct {
	Listener string `json:"listener"`
	Local    string `json:"local"`  // local IP the client connected to
	Remote   string `json:"remote"` // client source IP
	TLS      bool   `json:"tls"`
	// ClientCert is the verified client certificate (DER), if any.
	ClientCert []byte `json:"clientCert,omitempty"`
	User       string `json:"user"`
	Database   string `json:"database"`
	// Password is set on the second call, after a cleartext password request.
	Password *string `json:"password,omitempty"`
}

// Actions returned by the emulator.
const (
	ActionDeny        = "deny"        // send Message as a FATAL error
	ActionPassthrough = "passthrough" // relay to PostgreSQL over TCP (password auth)
	ActionPassword    = "password"    // ask for a cleartext password, then call again
	ActionTrust       = "trust"       // authenticated: log in over the unix socket
)

// LoginResponse is the emulator's decision.
type LoginResponse struct {
	Action  string `json:"action"`
	Code    string `json:"code,omitempty"` // SQLSTATE for deny
	Message string `json:"message,omitempty"`
}

// PGCommand is the command line the agent runs for PostgreSQL.
func PGCommand() []string {
	return []string{"docker-entrypoint.sh", "postgres",
		"-c", "listen_addresses=127.0.0.1",
		"-c", "unix_socket_directories=" + SocketDir,
		"-c", "hba_file=" + filepath.Join(Dir, HBAFile),
	}
}

// Main is the agent entry point.
func Main(ctx context.Context, _ []string) error {
	a := &agent{
		control: os.Getenv(EnvControl),
		key:     os.Getenv(EnvKey),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
	if a.control == "" {
		return errors.New(EnvControl + " is not set")
	}
	cmdline := PGCommand()
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopPG := func() error {
		_ = cmd.Process.Signal(syscall.SIGINT) // fast shutdown
		select {
		case err := <-exited:
			return err
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			return <-exited
		}
	}

	// Wait for the final server (the init-time temporary server listens on
	// the unix socket only).
	for !pgReady() {
		select {
		case err := <-exited:
			return fmt.Errorf("postgres exited during startup: %v", err)
		case <-ctx.Done():
			return stopPG()
		case <-time.After(50 * time.Millisecond):
		}
	}

	if err := a.loadTLS(); err != nil {
		_ = stopPG()
		return err
	}
	direct, err := net.Listen("tcp", fmt.Sprintf(":%d", PublicPort))
	if err != nil {
		_ = stopPG()
		return err
	}
	conn, err := net.Listen("tcp", fmt.Sprintf(":%d", ConnectPort))
	if err != nil {
		direct.Close()
		_ = stopPG()
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.serve(direct, ListenerDirect) }()
	go func() { defer wg.Done(); a.serve(conn, ListenerConnector) }()
	fmt.Fprintf(os.Stderr, "gcpemu sql-proxy: ready on :%d and :%d\n", PublicPort, ConnectPort)

	var pgErr error
	select {
	case <-ctx.Done():
		direct.Close()
		conn.Close()
		pgErr = stopPG()
	case pgErr = <-exited:
		direct.Close()
		conn.Close()
		if pgErr == nil {
			pgErr = errors.New("postgres exited")
		}
		return pgErr
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return pgErr
}

type agent struct {
	control string
	key     string
	http    *http.Client

	serverTLS    *tls.Config // :5432 after SSLRequest (client cert optional)
	connectorTLS *tls.Config // :3307 (client cert required)
}

func (a *agent) loadTLS() error {
	cert, err := tls.LoadX509KeyPair(filepath.Join(Dir, ServerCert), filepath.Join(Dir, ServerKey))
	if err != nil {
		return fmt.Errorf("load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(Dir, ClientCA))
	if err != nil {
		return fmt.Errorf("load client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("client CA: no certificates")
	}
	a.serverTLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}
	a.connectorTLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}
	return nil
}

func (a *agent) serve(l net.Listener, listener string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go a.handle(c, listener)
	}
}

// handle runs one client connection through the startup phase.
func (a *agent) handle(raw net.Conn, listener string) {
	defer raw.Close()
	req := LoginRequest{Listener: listener, Local: hostOf(raw.LocalAddr()), Remote: hostOf(raw.RemoteAddr())}
	var conn net.Conn = raw
	_ = raw.SetDeadline(time.Now().Add(60 * time.Second))
	if listener == ListenerConnector {
		tc := tls.Server(raw, a.connectorTLS)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
		req.TLS = true
		if pc := tc.ConnectionState().PeerCertificates; len(pc) > 0 {
			req.ClientCert = pc[0].Raw
		}
	}
	var st *startup
	for st == nil {
		m, err := readStartup(conn)
		if err != nil {
			return
		}
		switch m.code {
		case sslRequestCode:
			if req.TLS {
				_, _ = conn.Write([]byte{'N'})
				continue
			}
			if _, err := conn.Write([]byte{'S'}); err != nil {
				return
			}
			tc := tls.Server(conn, a.serverTLS)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			req.TLS = true
			if pc := tc.ConnectionState().PeerCertificates; len(pc) > 0 {
				req.ClientCert = pc[0].Raw
			}
		case gssEncRequestCode:
			_, _ = conn.Write([]byte{'N'})
		case cancelRequestCode:
			if pg, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", InternalPort), 5*time.Second); err == nil {
				_, _ = pg.Write(m.raw)
				pg.Close()
			}
			return
		default:
			if m.params == nil {
				_, _ = conn.Write(errorResponse("FATAL", "08P01", fmt.Sprintf("unsupported frontend protocol %d.%d", m.code>>16, m.code&0xffff)))
				return
			}
			st = m
		}
	}
	req.User, req.Database = st.params["user"], st.params["database"]
	if req.Database == "" {
		req.Database = req.User
	}
	resp, err := a.login(req)
	if err != nil {
		_, _ = conn.Write(errorResponse("FATAL", "08006", "gcpemu: cannot reach the emulator: "+err.Error()))
		return
	}
	if resp.Action == ActionPassword {
		if _, err := conn.Write(authCleartext()); err != nil {
			return
		}
		pw, err := readPassword(conn)
		if err != nil {
			return
		}
		req.Password = &pw
		if resp, err = a.login(req); err != nil {
			_, _ = conn.Write(errorResponse("FATAL", "08006", "gcpemu: cannot reach the emulator: "+err.Error()))
			return
		}
	}
	var pg net.Conn
	switch resp.Action {
	case ActionPassthrough:
		pg, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", InternalPort), 10*time.Second)
	case ActionTrust:
		pg, err = net.DialTimeout("unix", filepath.Join(SocketDir, fmt.Sprintf(".s.PGSQL.%d", InternalPort)), 10*time.Second)
	default:
		code := resp.Code
		if code == "" {
			code = "28000"
		}
		_, _ = conn.Write(errorResponse("FATAL", code, resp.Message))
		return
	}
	if err != nil {
		_, _ = conn.Write(errorResponse("FATAL", "57P03", "the database system is not available: "+err.Error()))
		return
	}
	defer pg.Close()
	_ = raw.SetDeadline(time.Time{})
	if _, err := pg.Write(st.raw); err != nil {
		return
	}
	splice(conn, pg)
}

// login asks the emulator for a decision.
func (a *agent) login(req LoginRequest) (*LoginResponse, error) {
	b, _ := json.Marshal(req)
	hr, err := http.NewRequest(http.MethodPost, a.control+"/login", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(KeyHeader, a.key)
	res, err := a.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, bytes.TrimSpace(body))
	}
	var out LoginResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// splice copies in both directions until either side closes.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	<-done
}

func hostOf(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		if v4 := ta.IP.To4(); v4 != nil {
			return v4.String()
		}
		return ta.IP.String()
	}
	h, _, _ := net.SplitHostPort(a.String())
	return h
}

// pgReady reports whether the final server accepts logins: a startup
// message over TCP is answered with an authentication request rather
// than an error such as "the database system is starting up" (57P03).
func pgReady() bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", InternalPort), time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	body := []byte{0, 3, 0, 0}
	for _, kv := range []string{"user", "cloudsqladmin", "database", "postgres", ""} {
		body = append(body, kv...)
		body = append(body, 0)
	}
	msg := make([]byte, 4, 4+len(body))
	msg[3] = byte(4 + len(body))
	if _, err := c.Write(append(msg, body...)); err != nil {
		return false
	}
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return false
	}
	return b[0] == 'R'
}
