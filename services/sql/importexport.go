package sql

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Import and export (FR-SQL-009, FR-INT-013): SQL dumps (pg_dump / psql)
// and CSV (COPY) to and from emulated Cloud Storage. Objects are read and
// written through the gcs service in-process, as the instance's service
// account, so bucket IAM applies as in GCP. URIs ending in ".gz" are
// compressed.

// parseGCSURI splits "gs://bucket/object".
func parseGCSURI(uri string) (bucket, object string, err error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	if !ok {
		return "", "", errInvalid("The URI %s is not a valid Cloud Storage location (gs://bucket/object).", uri)
	}
	bucket, object, _ = strings.Cut(rest, "/")
	if bucket == "" || object == "" {
		return "", "", errInvalid("The URI %s is not a valid Cloud Storage location (gs://bucket/object).", uri)
	}
	return bucket, object, nil
}

// bufferResponse is a minimal in-memory http.ResponseWriter.
type bufferResponse struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func (b *bufferResponse) Header() http.Header {
	if b.hdr == nil {
		b.hdr = http.Header{}
	}
	return b.hdr
}
func (b *bufferResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.body.Write(p)
}
func (b *bufferResponse) WriteHeader(code int) { b.code = code }

// gcs performs a JSON API request against the gcs service as principal.
func (s *Service) gcs(ctx context.Context, principal, method, path string, q url.Values, body []byte) ([]byte, error) {
	h, ok := s.peer("gcs").(http.Handler)
	if !ok {
		return nil, newOpError("INTERNAL_ERROR", "the Cloud Storage service (gcs) is not running; start the emulator with --services including gcs")
	}
	u := &url.URL{Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(emu.WithPrincipal(ctx, emu.Principal(principal)), method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = "storage.googleapis.com"
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	var rw bufferResponse
	h.ServeHTTP(&rw, req)
	if rw.code >= 300 {
		return nil, newOpError("ERROR_RDBMS", "Cloud Storage %s %s: HTTP %d: %s", method, path, rw.code, strings.TrimSpace(rw.body.String()))
	}
	return rw.body.Bytes(), nil
}

func (s *Service) readObject(ctx context.Context, rec *instanceRecord, uri string) ([]byte, error) {
	bucket, object, err := parseGCSURI(uri)
	if err != nil {
		return nil, err
	}
	b, err := s.gcs(ctx, "serviceAccount:"+rec.Instance.ServiceAccountEmailAddress, http.MethodGet,
		"/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(object), url.Values{"alt": {"media"}}, nil)
	if err != nil {
		return nil, err
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	}
	return b, nil
}

func (s *Service) writeObject(ctx context.Context, rec *instanceRecord, uri string, data []byte) error {
	bucket, object, err := parseGCSURI(uri)
	if err != nil {
		return err
	}
	if strings.HasSuffix(object, ".gz") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
		data = buf.Bytes()
	}
	_, err = s.gcs(ctx, "serviceAccount:"+rec.Instance.ServiceAccountEmailAddress, http.MethodPost,
		"/upload/storage/v1/b/"+url.PathEscape(bucket)+"/o", url.Values{"uploadType": {"media"}, "name": {object}}, data)
	return err
}

func (s *Service) exportInstance(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.export")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.InstancesExportRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	ec := req.ExportContext
	if ec == nil {
		apierr.Write(w, errInvalid("exportContext is required."))
		return
	}
	if _, _, err := parseGCSURI(ec.Uri); err != nil {
		apierr.Write(w, err)
		return
	}
	ec.Kind = "sql#exportContext"
	if ec.FileType == "" {
		ec.FileType = "SQL"
	}
	if ec.FileType != "SQL" && ec.FileType != "CSV" {
		apierr.Write(w, errInvalid("File type %s is not supported for PostgreSQL.", ec.FileType))
		return
	}
	if len(ec.Databases) != 1 && !(ec.FileType == "CSV" && len(ec.Databases) == 0) {
		apierr.Write(w, errInvalid("Exactly one database must be specified for a PostgreSQL export."))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	project, name := rec.Instance.Project, rec.Instance.Name
	db := "postgres"
	if len(ec.Databases) > 0 {
		db = ec.Databases[0]
	}
	if !s.databaseExists(project, name, db) {
		apierr.Write(w, errInvalid("database %q does not exist.", db))
		return
	}
	op := s.newOp(r.Context(), project, name, "EXPORT")
	op.ExportContext = ec
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		var out string
		var err error
		switch ec.FileType {
		case "CSV":
			if ec.CsvExportOptions == nil || strings.TrimSpace(ec.CsvExportOptions.SelectQuery) == "" {
				return newOpError("INVALID_REQUEST", "csvExportOptions.selectQuery is required for CSV exports")
			}
			q := strings.TrimRight(strings.TrimSpace(ec.CsvExportOptions.SelectQuery), ";")
			out, err = s.execSQL(ctx, project, name, db, "", "COPY ("+q+") TO STDOUT WITH (FORMAT csv);")
		default:
			args := []string{"pg_dump", "-h", "/var/run/postgresql", "-p", "5433", "-U", superuser, "--no-password", "--no-owner", "--no-privileges", "-d", db}
			if o := ec.SqlExportOptions; o != nil {
				if o.SchemaOnly {
					args = append(args, "--schema-only")
				}
				for _, t := range o.Tables {
					args = append(args, "-t", t)
				}
			}
			out, err = s.execIn(ctx, project, name, "", args...)
		}
		if err != nil {
			return err
		}
		return s.writeObject(ctx, rec, ec.Uri, []byte(out))
	}))
}

func (s *Service) importInstance(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.import")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.InstancesImportRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	ic := req.ImportContext
	if ic == nil {
		apierr.Write(w, errInvalid("importContext is required."))
		return
	}
	if _, _, err := parseGCSURI(ic.Uri); err != nil {
		apierr.Write(w, err)
		return
	}
	ic.Kind = "sql#importContext"
	if ic.FileType == "" {
		ic.FileType = "SQL"
	}
	if ic.FileType != "SQL" && ic.FileType != "CSV" {
		apierr.Write(w, errInvalid("File type %s is not supported for PostgreSQL.", ic.FileType))
		return
	}
	if ic.Database == "" {
		apierr.Write(w, errInvalid("importContext.database is required for PostgreSQL."))
		return
	}
	project, name := rec.Instance.Project, rec.Instance.Name
	if !s.databaseExists(project, name, ic.Database) {
		apierr.Write(w, errInvalid("database %q does not exist.", ic.Database))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op := s.newOp(r.Context(), project, name, "IMPORT")
	op.ImportContext = ic
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		data, err := s.readObject(ctx, rec, ic.Uri)
		if err != nil {
			return err
		}
		user := orDefault(ic.ImportUser, "postgres")
		switch ic.FileType {
		case "CSV":
			o := ic.CsvImportOptions
			if o == nil || o.Table == "" {
				return newOpError("INVALID_REQUEST", "csvImportOptions.table is required for CSV imports")
			}
			target := o.Table
			if len(o.Columns) > 0 {
				cols := make([]string, len(o.Columns))
				for i, c := range o.Columns {
					cols[i] = quoteIdent(c)
				}
				target += " (" + strings.Join(cols, ", ") + ")"
			}
			_, err = s.execIn(ctx, project, name, string(data), "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1",
				"-h", "/var/run/postgresql", "-p", "5433", "-U", user, "-d", ic.Database,
				"-c", fmt.Sprintf("COPY %s FROM STDIN WITH (FORMAT csv)", target))
		default:
			_, err = s.execIn(ctx, project, name, string(data), "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1",
				"-h", "/var/run/postgresql", "-p", "5433", "-U", user, "-d", ic.Database, "-f", "-")
		}
		return err
	}))
}
