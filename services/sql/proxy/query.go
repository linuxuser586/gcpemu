package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// QueryName is the agent that runs instances.executeSql (the Cloud SQL
// Data API) inside an instance's container: the emulator execs it with a
// QueryRequest on stdin and reads a QueryResponse from stdout. It logs in
// over the unix socket (trust), as the emulator's admin commands do, and
// sends the statements as one simple-protocol query, so that PostgreSQL
// runs them in one implicit transaction and reports each statement's
// columns, types and NULLs exactly.
const QueryName = "sql-query"

// EnvQueryTimeout is how long the agent lets the statements run.
const EnvQueryTimeout = "GCPEMU_SQL_QUERY_TIMEOUT"

// MaxMessages is how many notices a response carries, as in Cloud SQL.
const MaxMessages = 10

// QueryRequest is what the emulator asks the agent to run.
type QueryRequest struct {
	User     string `json:"user"`
	Database string `json:"database"`
	SQL      string `json:"sql"`
	// RowLimit caps the rows returned per statement (0: no cap).
	RowLimit int64 `json:"rowLimit,omitempty"`
	// MaxBytes caps the size of all values returned; beyond it the result
	// is truncated (AllowPartial) or the request fails.
	MaxBytes     int64 `json:"maxBytes"`
	AllowPartial bool  `json:"allowPartial,omitempty"`
}

// QueryResponse has the shape of sqladmin's SqlInstancesExecuteSqlResponse
// without its metadata, which the emulator adds.
type QueryResponse struct {
	Messages []QueryMessage `json:"messages,omitempty"`
	Results  []QueryResult  `json:"results,omitempty"`
	Status   *QueryStatus   `json:"status,omitempty"`
}

type QueryMessage struct {
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

type QueryResult struct {
	Columns       []QueryColumn `json:"columns,omitempty"`
	Rows          []QueryRow    `json:"rows,omitempty"`
	Message       string        `json:"message,omitempty"`
	PartialResult bool          `json:"partialResult,omitempty"`
}

type QueryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	oid  uint32
}

type QueryRow struct {
	Values []QueryValue `json:"values"`
}

type QueryValue struct {
	Value     string `json:"value,omitempty"`
	NullValue bool   `json:"nullValue,omitempty"`
}

// QueryStatus is a google.rpc.Status.
type QueryStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// QueryMain is the sql-query agent's entry point.
func QueryMain(ctx context.Context, _ []string) error {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var req QueryRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return err
	}
	if d, err := time.ParseDuration(os.Getenv(EnvQueryTimeout)); err == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	return json.NewEncoder(os.Stdout).Encode(RunQuery(ctx, SocketDir, InternalPort, &req))
}

// RunQuery runs req against the server listening on host:port.
func RunQuery(ctx context.Context, host string, port int, req *QueryRequest) *QueryResponse {
	resp := &QueryResponse{}
	cfg, err := pgconn.ParseConfig("")
	if err != nil {
		resp.Status = &QueryStatus{Code: 13, Message: err.Error()}
		return resp
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Database = host, uint16(port), req.User, req.Database
	cfg.TLSConfig, cfg.Fallbacks = nil, nil
	cfg.RuntimeParams = map[string]string{"application_name": "cloudsql-data-api"}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if len(resp.Messages) < MaxMessages {
			resp.Messages = append(resp.Messages, QueryMessage{Message: n.Message, Severity: n.Severity})
		}
	}
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		resp.Status = pgStatus(err)
		return resp
	}
	defer conn.Close(context.Background())

	var size int64
	full := false
	mrr := conn.Exec(ctx, req.SQL)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		var res QueryResult
		for _, f := range rr.FieldDescriptions() {
			res.Columns = append(res.Columns, QueryColumn{Name: f.Name, oid: f.DataTypeOID})
		}
		for rr.NextRow() {
			if full || (req.RowLimit > 0 && int64(len(res.Rows)) >= req.RowLimit) {
				res.PartialResult = true
				continue
			}
			row := QueryRow{Values: make([]QueryValue, 0, len(rr.Values()))}
			for _, v := range rr.Values() {
				size += int64(len(v))
				if v == nil {
					row.Values = append(row.Values, QueryValue{NullValue: true})
				} else {
					row.Values = append(row.Values, QueryValue{Value: string(v)})
				}
			}
			if req.MaxBytes > 0 && size > req.MaxBytes {
				full, res.PartialResult = true, true
				continue
			}
			res.Rows = append(res.Rows, row)
		}
		tag, err := rr.Close()
		if err != nil {
			break // reported by mrr.Close
		}
		if len(res.Columns) == 0 && tag.String() == "" {
			continue // an empty statement
		}
		res.Message = tag.String()
		resp.Results = append(resp.Results, res)
	}
	if err := mrr.Close(); err != nil {
		// As in Cloud SQL, a failed request returns no results; PostgreSQL
		// has rolled back the implicit transaction.
		resp.Results = nil
		resp.Status = pgStatus(err)
		return resp
	}
	if full && !req.AllowPartial {
		resp.Results = nil
		resp.Status = &QueryStatus{Code: 8, Message: fmt.Sprintf("The result exceeds the maximum response size of %d MB. Set partialResultMode to ALLOW_PARTIAL_RESULT to get a truncated result.", req.MaxBytes>>20)}
		return resp
	}
	nameTypes(ctx, conn, resp.Results)
	return resp
}

// nameTypes fills in the columns' type names (pg_type.typname, upper-cased
// as database/sql drivers report them), looking up the OIDs it does not
// know, such as those of enums and domains.
func nameTypes(ctx context.Context, conn *pgconn.PgConn, results []QueryResult) {
	names := map[uint32]string{}
	var unknown []string
	for _, r := range results {
		for _, c := range r.Columns {
			if _, ok := names[c.oid]; ok {
				continue
			}
			names[c.oid] = ""
			unknown = append(unknown, strconv.FormatUint(uint64(c.oid), 10))
		}
	}
	if len(unknown) > 0 {
		q := "SELECT oid::text, typname FROM pg_catalog.pg_type WHERE oid IN (" + strings.Join(unknown, ",") + ")"
		if rows, err := conn.Exec(ctx, q).ReadAll(); err == nil && len(rows) == 1 {
			for _, row := range rows[0].Rows {
				oid, _ := strconv.ParseUint(string(row[0]), 10, 32)
				names[uint32(oid)] = strings.ToUpper(string(row[1]))
			}
		}
	}
	for i := range results {
		for j := range results[i].Columns {
			c := &results[i].Columns[j]
			c.Type = names[c.oid]
			if c.Type == "" {
				c.Type = "UNKNOWN"
			}
		}
	}
}

// pgStatus maps a PostgreSQL error to a google.rpc.Status by SQLSTATE.
func pgStatus(err error) *QueryStatus {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
			return &QueryStatus{Code: 4, Message: "The request was canceled after 30 seconds."}
		}
		return &QueryStatus{Code: 14, Message: err.Error()}
	}
	code := 2 // UNKNOWN
	switch {
	case pe.Code == "42501" || strings.HasPrefix(pe.Code, "28"):
		code = 7 // PERMISSION_DENIED
	case pe.Code == "57014":
		code = 4 // DEADLINE_EXCEEDED
	case strings.HasPrefix(pe.Code, "42"), strings.HasPrefix(pe.Code, "22"):
		code = 3 // INVALID_ARGUMENT
	case strings.HasPrefix(pe.Code, "23"), strings.HasPrefix(pe.Code, "25"), strings.HasPrefix(pe.Code, "3D"):
		code = 9 // FAILED_PRECONDITION
	case strings.HasPrefix(pe.Code, "53"):
		code = 8 // RESOURCE_EXHAUSTED
	}
	return &QueryStatus{Code: code, Message: fmt.Sprintf("%s: %s (SQLSTATE %s)", pe.Severity, pe.Message, pe.Code)}
}
