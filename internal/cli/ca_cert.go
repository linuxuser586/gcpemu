package cli

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// trustCert is the CA certificate `ca install` and `ca uninstall` add to
// or remove from the trust stores: the instance's own *ca.CA, or a
// certificate given with --cert (#496).
type trustCert interface {
	PEM() []byte
	Path() string
	Certificate() *x509.Certificate
}

// certFile is a CA certificate given with --cert, such as a Containerized
// Instance's from `docker exec gcpemu /gcpemu ca print`. It has no key and
// no data directory; path is "" when it came from stdin, until ensurePath.
type certFile struct {
	pem  []byte
	cert *x509.Certificate
	path string
}

func (f *certFile) PEM() []byte                    { return append([]byte(nil), f.pem...) }
func (f *certFile) Path() string                   { return f.path }
func (f *certFile) Certificate() *x509.Certificate { return f.cert }

// readCertFile reads the --cert certificate from name, or from stdin when
// name is "-". It must be exactly one PEM CERTIFICATE block holding a CA
// certificate; its subject is not checked.
func readCertFile(name string, stdin io.Reader) (*certFile, error) {
	var b []byte
	var err error
	f := &certFile{}
	if name == "-" {
		b, err = io.ReadAll(stdin)
	} else {
		if f.path, err = filepath.Abs(name); err == nil {
			b, err = os.ReadFile(f.path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("--cert: %w", err)
	}
	block, rest := pem.Decode(b)
	switch {
	case block == nil:
		return nil, fmt.Errorf("--cert %s: no PEM block found; want one PEM CERTIFICATE block", name)
	case block.Type != "CERTIFICATE":
		return nil, fmt.Errorf("--cert %s: holds a PEM %s block; want one PEM CERTIFICATE block", name, block.Type)
	}
	if next, _ := pem.Decode(rest); next != nil {
		return nil, fmt.Errorf("--cert %s: more than one PEM block; want exactly one CERTIFICATE", name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("--cert %s: not an X.509 certificate: %v", name, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("--cert %s: not a CA certificate", name)
	}
	f.cert, f.pem = cert, pem.EncodeToMemory(block)
	return f, nil
}

// stdinCertPath stands in for a stdin certificate's temporary file in a
// dry run, which creates none.
const stdinCertPath = "<temporary file holding the --cert - certificate>"

// ensurePath gives a stdin certificate a file when need is set (the macOS
// keychain and Windows certutil take a path); cleanup removes it. A dry
// run gets stdinCertPath instead. A certificate given by path is never
// copied or removed.
func (f *certFile) ensurePath(need, dryRun bool) (cleanup func(), err error) {
	if !need || f.path != "" {
		return func() {}, nil
	}
	if dryRun {
		f.path = stdinCertPath
		return func() {}, nil
	}
	tmp, err := os.CreateTemp("", "gcpemu-ca-*.pem")
	if err != nil {
		return nil, fmt.Errorf("--cert: %w", err)
	}
	_, err = tmp.Write(f.pem)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("--cert: %w", err)
	}
	f.path = tmp.Name()
	return func() { os.Remove(tmp.Name()) }, nil
}
