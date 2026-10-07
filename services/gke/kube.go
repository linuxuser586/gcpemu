package gke

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A small hand-rolled Kubernetes API client: the emulator talks to each
// cluster as system:admin (k3s's admin client certificate) to wait for
// readiness, manage nodes, resolve Workload Identity and sync NEGs.

// adminCreds is the admin access k3s writes to /etc/rancher/k3s/k3s.yaml.
type adminCreds struct {
	CA   []byte // PEM
	Cert []byte // PEM
	Key  []byte // PEM
}

// parseK3sKubeconfig extracts the admin credentials.
func parseK3sKubeconfig(b []byte) (adminCreds, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				CAData string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				CertData string `yaml:"client-certificate-data"`
				KeyData  string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(b, &kc); err != nil {
		return adminCreds{}, err
	}
	if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
		return adminCreds{}, errors.New("k3s kubeconfig has no cluster or user")
	}
	var c adminCreds
	var err error
	if c.CA, err = base64.StdEncoding.DecodeString(kc.Clusters[0].Cluster.CAData); err != nil {
		return c, err
	}
	if c.Cert, err = base64.StdEncoding.DecodeString(kc.Users[0].User.CertData); err != nil {
		return c, err
	}
	if c.Key, err = base64.StdEncoding.DecodeString(kc.Users[0].User.KeyData); err != nil {
		return c, err
	}
	return c, nil
}

type kubeClient struct {
	base string
	hc   *http.Client
	ca   []byte
}

func newKubeClient(server string, creds adminCreds) (*kubeClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(creds.CA) {
		return nil, errors.New("invalid cluster CA")
	}
	cert, err := tls.X509KeyPair(creds.Cert, creds.Key)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &kubeClient{base: server, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}, ca: creds.CA}, nil
}

// kubeError is a non-2xx API response.
type kubeError struct {
	Status int
	Body   string
}

func (e *kubeError) Error() string { return fmt.Sprintf("kubernetes API %d: %s", e.Status, e.Body) }

func isKubeNotFound(err error) bool {
	var ke *kubeError
	return errors.As(err, &ke) && ke.Status == http.StatusNotFound
}

func (k *kubeClient) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode/100 != 2 {
		return &kubeError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (k *kubeClient) get(ctx context.Context, path string, out any) error {
	return k.do(ctx, http.MethodGet, path, "", nil, out)
}

func (k *kubeClient) mergePatch(ctx context.Context, path string, patch any) error {
	return k.do(ctx, http.MethodPatch, path, "application/merge-patch+json", patch, nil)
}

func (k *kubeClient) delete(ctx context.Context, path string) error {
	err := k.do(ctx, http.MethodDelete, path, "", nil, nil)
	if isKubeNotFound(err) {
		return nil
	}
	return err
}

// ready reports whether /readyz answers ok.
func (k *kubeClient) ready(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, k.base+"/readyz", nil)
	resp, err := k.hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---- minimal API types ----

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
	} `json:"ownerReferences,omitempty"`
}

type kubeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

type kubeNode struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		PodCIDR       string      `json:"podCIDR"`
		Unschedulable bool        `json:"unschedulable"`
		Taints        []kubeTaint `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
}

func (n *kubeNode) ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

type kubePod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName           string `json:"nodeName"`
		ServiceAccountName string `json:"serviceAccountName"`
		HostNetwork        bool   `json:"hostNetwork"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

type kubeServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	Protocol   string `json:"protocol"`
	TargetPort any    `json:"targetPort"`
}

type kubeService struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Type  string            `json:"type"`
		Ports []kubeServicePort `json:"ports"`
	} `json:"spec"`
}

type kubeEndpointSlice struct {
	Metadata    objectMeta `json:"metadata"`
	AddressType string     `json:"addressType"`
	Endpoints   []struct {
		Addresses  []string `json:"addresses"`
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
		NodeName string `json:"nodeName"`
	} `json:"endpoints"`
	Ports []struct {
		Name     *string `json:"name"`
		Port     *int    `json:"port"`
		Protocol string  `json:"protocol"`
	} `json:"ports"`
}

type kubeList[T any] struct {
	Items []T `json:"items"`
}
