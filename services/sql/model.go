package sql

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces.
const (
	nsInstances = "sql/instances"  // P/I → instanceRecord
	nsDatabases = "sql/databases"  // P/I/D → sqladmin.Database
	nsUsers     = "sql/users"      // P/I/U → userRecord
	nsOps       = "sql/operations" // P/OP → sqladmin.Operation
	nsSSLCerts  = "sql/sslcerts"   // P/I/SHA1 → sqladmin.SslCert
	nsBackups   = "sql/backupruns" // P/I/ID → backupRecord
)

// API identifiers.
const (
	apiHost   = "sqladmin.googleapis.com"
	linkBase  = "https://sqladmin.googleapis.com/sql/v1beta4/"
	linkV1    = "https://sqladmin.googleapis.com/v1/"
	resRoot   = "//cloudsql.googleapis.com/"
	crmRoot   = "//cloudresourcemanager.googleapis.com/projects/"
	errDomain = "sqladmin.googleapis.com"
)

// instanceRecord is the stored state of one Cloud SQL instance: the API
// resource plus emulator-internal fields that are never returned.
type instanceRecord struct {
	Instance *sqladmin.DatabaseInstance `json:"instance"`

	RootPassword string `json:"rootPassword,omitempty"`
	// AgentKey authenticates the in-container agent to the emulator.
	AgentKey string `json:"agentKey"`
	// PKI (FR-SQL-005/007): server CA + server cert, client CA.
	ServerCAPEM    string `json:"serverCaPem"`
	ServerCAKeyPEM string `json:"serverCaKeyPem"`
	ServerCertPEM  string `json:"serverCertPem"`
	ServerKeyPEM   string `json:"serverKeyPem"`
	ClientCAPEM    string `json:"clientCaPem"`
	ClientCAKeyPEM string `json:"clientCaKeyPem"`

	// HostPort is the 127.0.0.1 port published for 5432 (NFR-PORT-003).
	HostPort int `json:"hostPort,omitempty"`
	// PublicIP / PrivateIP are the container addresses on the external and
	// private services networks.
	PublicIP   string         `json:"publicIp,omitempty"`
	PrivateIP  string         `json:"privateIp,omitempty"`
	PrivateNet *emu.SubnetNet `json:"privateNet,omitempty"`
	// AppliedFlags are the database flags currently applied to PostgreSQL.
	AppliedFlags map[string]string `json:"appliedFlags,omitempty"`
	// SeedHash records the init SQL applied by the seed (FR-CORE-011).
	SeedHash string `json:"seedHash,omitempty"`
}

// userRecord is a stored user (the password is kept for re-creation after
// restore and never returned).
type userRecord struct {
	User     *sqladmin.User `json:"user"`
	Password string         `json:"password,omitempty"`
}

// backupRecord is a stored backup run and the file holding its dump.
type backupRecord struct {
	Run       *sqladmin.BackupRun  `json:"run"`
	File      string               `json:"file,omitempty"`
	Databases []*sqladmin.Database `json:"databases,omitempty"`
	Users     []*userRecord        `json:"users,omitempty"`
}

// instKey is the store key "P/I".
func instKey(project, name string) string { return project + "/" + name }

// childKey is "P/I/X".
func childKey(project, inst, name string) string { return project + "/" + inst + "/" + name }

// splitKey splits "P/I".
func splitKey(k string) (project, name string) {
	project, name, _ = strings.Cut(k, "/")
	return
}

// instanceResource is the full resource name used for IAM checks.
func instanceResource(project, name string) string {
	return resRoot + "projects/" + project + "/instances/" + name
}

// projectResource is the resource for project-scoped permission checks.
func projectResource(project string) string { return crmRoot + project }

func instanceLink(project, name string) string {
	return linkBase + "projects/" + project + "/instances/" + name
}

func opLink(project, name string) string {
	return linkBase + "projects/" + project + "/operations/" + name
}

var instanceNameRe = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// validInstanceName follows GCP's rules: lowercase letters, digits and
// hyphens, starting with a letter; project ID + name at most 98 characters.
func validInstanceName(project, name string) bool {
	return instanceNameRe.MatchString(name) && len(project)+len(name) <= 98
}

// etagOf derives an etag from a resource's JSON.
func etagOf(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// getInstance loads an instance record.
func getInstance(tx store.Tx, project, name string) (*instanceRecord, bool) {
	var rec instanceRecord
	if store.GetJSON(tx, nsInstances, instKey(project, name), &rec) != nil || rec.Instance == nil {
		return nil, false
	}
	return &rec, true
}

// sqlRegions are the regions Cloud SQL serves (FR-CORE-021).
var sqlRegions = map[string]bool{
	"africa-south1": true, "asia-east1": true, "asia-east2": true, "asia-northeast1": true,
	"asia-northeast2": true, "asia-northeast3": true, "asia-south1": true, "asia-south2": true,
	"asia-southeast1": true, "asia-southeast2": true, "asia-southeast3": true, "australia-southeast1": true,
	"australia-southeast2": true, "europe-central2": true, "europe-north1": true, "europe-north2": true,
	"europe-southwest1": true, "europe-west1": true, "europe-west2": true, "europe-west3": true,
	"europe-west4": true, "europe-west6": true, "europe-west8": true, "europe-west9": true,
	"europe-west10": true, "europe-west12": true, "me-central1": true, "me-central2": true,
	"me-west1": true, "northamerica-northeast1": true, "northamerica-northeast2": true,
	"northamerica-south1": true, "southamerica-east1": true, "southamerica-west1": true,
	"us-central1": true, "us-east1": true, "us-east4": true, "us-east5": true, "us-south1": true,
	"us-west1": true, "us-west2": true, "us-west3": true, "us-west4": true,
}

const defaultRegion = "us-central1"

// scanJSON decodes every value under prefix, in key order.
func scanJSON[T any](tx store.Tx, ns, prefix string, fn func(key string, v *T)) {
	tx.Scan(ns, prefix, func(k string, b []byte) bool {
		v := new(T)
		if json.Unmarshal(b, v) == nil {
			fn(k, v)
		}
		return true
	})
}
