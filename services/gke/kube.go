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
	"net"
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

// newKubeClient dials server with dial (the ContainerDialer) and verifies
// its certificate for name (the address the API server advertises, when
// server is another one).
func newKubeClient(server, name string, creds adminCreds, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*kubeClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(creds.CA) {
		return nil, errors.New("invalid cluster CA")
	}
	cert, err := tls.X509KeyPair(creds.Cert, creds.Key)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext:         dial,
		TLSClientConfig:     &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, ServerName: name},
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

func (k *kubeClient) create(ctx context.Context, path string, obj, out any) error {
	return k.do(ctx, http.MethodPost, path, "", obj, out)
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

// ready reports whether /readyz answers ok and the controllers have
// created the default namespace's service account, without which pods
// can't be created (a GKE cluster takes workloads once it is RUNNING).
func (k *kubeClient) ready(ctx context.Context) bool {
	for _, p := range []string{"/readyz", "/api/v1/namespaces/default/serviceaccounts/default"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, k.base+p, nil)
		resp, err := k.hc.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
	}
	return true
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
	Generation        int64             `json:"generation,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
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

// ready reports whether the node is Ready and the node controller has
// lifted the not-ready and unreachable taints it adds at registration,
// which lag the Ready condition and keep pods off the node.
func (n *kubeNode) ready() bool {
	for _, t := range n.Spec.Taints {
		if t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable" {
			return false
		}
	}
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
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	AppProtocol string `json:"appProtocol,omitempty"`
	TargetPort  any    `json:"targetPort"`
	NodePort    int    `json:"nodePort,omitempty"`
}

type kubeService struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Type         string            `json:"type"`
		ClusterIP    string            `json:"clusterIP,omitempty"`
		ExternalName string            `json:"externalName,omitempty"`
		Ports        []kubeServicePort `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip,omitempty"`
				Hostname string `json:"hostname,omitempty"`
			} `json:"ingress,omitempty"`
		} `json:"loadBalancer"`
	} `json:"status"`
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

type kubeSecret struct {
	Metadata objectMeta        `json:"metadata"`
	Type     string            `json:"type"`
	Data     map[string][]byte `json:"data"`
}

// lbIngressStatus is a status.loadBalancer.ingress entry.
type lbIngressStatus struct {
	IP       string `json:"ip,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

type ingressBackend struct {
	Service *struct {
		Name string `json:"name"`
		Port struct {
			Number int    `json:"number,omitempty"`
			Name   string `json:"name,omitempty"`
		} `json:"port"`
	} `json:"service,omitempty"`
}

type kubeIngress struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		IngressClassName *string         `json:"ingressClassName,omitempty"`
		DefaultBackend   *ingressBackend `json:"defaultBackend,omitempty"`
		TLS              []struct {
			Hosts      []string `json:"hosts,omitempty"`
			SecretName string   `json:"secretName,omitempty"`
		} `json:"tls,omitempty"`
		Rules []struct {
			Host string `json:"host,omitempty"`
			HTTP *struct {
				Paths []struct {
					Path     string         `json:"path,omitempty"`
					PathType string         `json:"pathType,omitempty"`
					Backend  ingressBackend `json:"backend"`
				} `json:"paths"`
			} `json:"http,omitempty"`
		} `json:"rules,omitempty"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []lbIngressStatus `json:"ingress,omitempty"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

// Gateway API (gateway.networking.k8s.io/v1).

type gwCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type kubeGatewayClass struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ControllerName string `json:"controllerName"`
	} `json:"spec"`
	Status struct {
		Conditions []gwCondition `json:"conditions,omitempty"`
	} `json:"status"`
}

type gwCertRef struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type gwListener struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname,omitempty"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	TLS      *struct {
		Mode            string            `json:"mode,omitempty"`
		CertificateRefs []gwCertRef       `json:"certificateRefs,omitempty"`
		Options         map[string]string `json:"options,omitempty"`
	} `json:"tls,omitempty"`
	AllowedRoutes *struct {
		Namespaces *struct {
			From string `json:"from,omitempty"`
		} `json:"namespaces,omitempty"`
	} `json:"allowedRoutes,omitempty"`
}

type gwKind struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
}

type gwListenerStatus struct {
	Name           string        `json:"name"`
	SupportedKinds []gwKind      `json:"supportedKinds"`
	AttachedRoutes int           `json:"attachedRoutes"`
	Conditions     []gwCondition `json:"conditions"`
}

type gwAddress struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
}

type gwStatus struct {
	Addresses  []gwAddress        `json:"addresses"`
	Conditions []gwCondition      `json:"conditions,omitempty"`
	Listeners  []gwListenerStatus `json:"listeners,omitempty"`
}

type kubeGateway struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		GatewayClassName string       `json:"gatewayClassName"`
		Listeners        []gwListener `json:"listeners"`
		Addresses        []gwAddress  `json:"addresses,omitempty"`
	} `json:"spec"`
	Status gwStatus `json:"status"`
}

type gwParentRef struct {
	Group       *string `json:"group,omitempty"`
	Kind        *string `json:"kind,omitempty"`
	Namespace   *string `json:"namespace,omitempty"`
	Name        string  `json:"name"`
	SectionName *string `json:"sectionName,omitempty"`
	Port        *int    `json:"port,omitempty"`
}

type gwMatchValue struct {
	Type  string `json:"type,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value"`
}

type gwHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gwHeaderModifier struct {
	Set    []gwHeader `json:"set,omitempty"`
	Add    []gwHeader `json:"add,omitempty"`
	Remove []string   `json:"remove,omitempty"`
}

type gwPathModifier struct {
	Type               string `json:"type"`
	ReplaceFullPath    string `json:"replaceFullPath,omitempty"`
	ReplacePrefixMatch string `json:"replacePrefixMatch,omitempty"`
}

type gwFilter struct {
	Type                   string            `json:"type"`
	RequestHeaderModifier  *gwHeaderModifier `json:"requestHeaderModifier,omitempty"`
	ResponseHeaderModifier *gwHeaderModifier `json:"responseHeaderModifier,omitempty"`
	RequestRedirect        *struct {
		Scheme     *string         `json:"scheme,omitempty"`
		Hostname   *string         `json:"hostname,omitempty"`
		Path       *gwPathModifier `json:"path,omitempty"`
		Port       *int            `json:"port,omitempty"`
		StatusCode *int            `json:"statusCode,omitempty"`
	} `json:"requestRedirect,omitempty"`
	URLRewrite *struct {
		Hostname *string         `json:"hostname,omitempty"`
		Path     *gwPathModifier `json:"path,omitempty"`
	} `json:"urlRewrite,omitempty"`
}

type gwBackendRef struct {
	Group     *string `json:"group,omitempty"`
	Kind      *string `json:"kind,omitempty"`
	Name      string  `json:"name"`
	Namespace *string `json:"namespace,omitempty"`
	Port      *int    `json:"port,omitempty"`
	Weight    *int    `json:"weight,omitempty"`
}

type gwRouteMatch struct {
	Path        *gwMatchValue  `json:"path,omitempty"`
	Headers     []gwMatchValue `json:"headers,omitempty"`
	QueryParams []gwMatchValue `json:"queryParams,omitempty"`
	Method      string         `json:"method,omitempty"`
}

type gwRouteRule struct {
	Matches     []gwRouteMatch `json:"matches,omitempty"`
	Filters     []gwFilter     `json:"filters,omitempty"`
	BackendRefs []gwBackendRef `json:"backendRefs,omitempty"`
}

type gwParentStatus struct {
	ParentRef      gwParentRef   `json:"parentRef"`
	ControllerName string        `json:"controllerName"`
	Conditions     []gwCondition `json:"conditions"`
}

type kubeHTTPRoute struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []gwParentRef `json:"parentRefs,omitempty"`
		Hostnames  []string      `json:"hostnames,omitempty"`
		Rules      []gwRouteRule `json:"rules,omitempty"`
	} `json:"spec"`
	Status struct {
		Parents []gwParentStatus `json:"parents"`
	} `json:"status"`
}

type kubeList[T any] struct {
	Items []T `json:"items"`
}
