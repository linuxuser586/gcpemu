package sql

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// instances.executeSql, the Cloud SQL Data API: the instance must allow it
// (settings.dataApiAccess ALLOW_DATA_API) and be running, and the caller
// needs cloudsql.instances.executeSql. The statements run as one database
// user: the caller's IAM database user with autoIamAuthn, otherwise the
// named user, whose password, when passwordSecretVersion names a Secret
// Manager version, must be the user's. Unlike Cloud SQL, the emulator also
// accepts a named user without a password, so that local tools (such as
// the console's query runner) need no secret.

const (
	dataAPIMaxRequest  = 512 << 10
	dataAPIMaxResponse = 10 << 20
	dataAPITimeout     = 30 * time.Second
)

var applicationRe = regexp.MustCompile(`^[A-Za-z0-9_-]{0,32}$`)

func (s *Service) executeSQL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rec, err := s.instanceFor(r, "cloudsql.instances.executeSql")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var p sqladmin.ExecuteSqlPayload
	if err := decode(r, &p, false); err != nil {
		apierr.Write(w, err)
		return
	}
	user, err := s.dataAPIUser(ctx, rec, &p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	resp, err := s.runDataAPI(ctx, rec, user, &p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, resp)
}

// dataAPIUser validates the request and returns the database user it runs as.
func (s *Service) dataAPIUser(ctx context.Context, rec *instanceRecord, p *sqladmin.ExecuteSqlPayload) (string, error) {
	in := rec.Instance
	switch {
	case strings.TrimSpace(p.SqlStatement) == "":
		return "", errInvalid("sqlStatement is required.")
	case len(p.SqlStatement) > dataAPIMaxRequest:
		return "", errInvalid("The request exceeds the 0.5 MB limit of the Data API.")
	case p.RowLimit < 0:
		return "", errInvalid("rowLimit must not be negative.")
	case !applicationRe.MatchString(p.Application):
		return "", errInvalid("application must be at most 32 letters, digits, dashes and underscores.")
	case p.AutoIamAuthn && p.PasswordSecretVersion != "":
		return "", errInvalid("Only one of autoIamAuthn and passwordSecretVersion can be set.")
	}
	switch p.PartialResultMode {
	case "", "PARTIAL_RESULT_MODE_UNSPECIFIED", "FAIL_PARTIAL_RESULT", "ALLOW_PARTIAL_RESULT":
	default:
		return "", errInvalid("Invalid partialResultMode %s.", p.PartialResultMode)
	}
	if in.Settings == nil || in.Settings.DataApiAccess != "ALLOW_DATA_API" {
		return "", apierr.FailedPrecondition("The Data API is not enabled for instance %s. Set settings.dataApiAccess to ALLOW_DATA_API, for example with gcloud sql instances patch %s --data-api-access=ALLOW_DATA_API.", in.Name, in.Name).
			WithLegacy("failedPrecondition")
	}
	if err := requireRunnable(rec); err != nil {
		return "", err
	}
	if p.AutoIamAuthn {
		if flagMap(in.Settings)["cloudsql.iam_authentication"] != "on" {
			return "", apierr.FailedPrecondition("IAM database authentication is not enabled on instance %s: set the cloudsql.iam_authentication flag to on.", in.Name)
		}
		principal := emu.PrincipalFrom(ctx)
		for _, u := range s.instanceUsers(in.Project, in.Name) {
			if iamUserMatches(u.User, principal) {
				return u.User.Name, nil
			}
		}
		return "", apierr.PermissionDenied("%s is not an IAM database user of instance %s.", principal, in.Name)
	}
	if p.User == "" {
		return "", errInvalid("user is required unless autoIamAuthn is set.")
	}
	u, ok := s.findUser(in.Project, in.Name, p.User)
	if !ok {
		return "", errUserNotFound(p.User)
	}
	if p.PasswordSecretVersion != "" {
		if u.User.Type != "" && u.User.Type != "BUILT_IN" {
			return "", errInvalid("passwordSecretVersion applies to built-in users only.")
		}
		if err := s.checkSecretPassword(ctx, in, u, p.PasswordSecretVersion); err != nil {
			return "", err
		}
	}
	return u.User.Name, nil
}

var secretVersionRe = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/secrets/[^/]+/versions/[^/]+$`)

// checkSecretPassword compares a regional secret version's payload with
// the user's password.
func (s *Service) checkSecretPassword(ctx context.Context, in *sqladmin.DatabaseInstance, u *userRecord, name string) error {
	m := secretVersionRe.FindStringSubmatch(name)
	if m == nil {
		return errInvalid("passwordSecretVersion must be projects/{project}/locations/{location}/secrets/{secret}/versions/{version}.")
	}
	if m[1] != in.Region {
		return errInvalid("The secret must be in the instance's region (%s).", in.Region)
	}
	sa, ok := s.peer("secrets").(emu.SecretAccessor)
	if !ok {
		return apierr.FailedPrecondition("passwordSecretVersion needs the Secret Manager service (secrets), which is not enabled.")
	}
	payload, _, err := sa.AccessSecretVersion(ctx, name)
	if err != nil {
		return err
	}
	if u.Password == "" || string(payload) != u.Password {
		return apierr.PermissionDenied("password authentication failed for user %q", u.User.Name)
	}
	return nil
}

// instanceUsers lists an instance's stored users.
func (s *Service) instanceUsers(project, inst string) []*userRecord {
	var out []*userRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsUsers, project+"/"+inst+"/", func(_ string, u *userRecord) {
			if u.User != nil {
				out = append(out, u)
			}
		})
		return nil
	})
	return out
}

// runDataAPI runs the statements with the sql-query agent in the instance's
// container.
func (s *Service) runDataAPI(ctx context.Context, rec *instanceRecord, user string, p *sqladmin.ExecuteSqlPayload) (*sqladmin.SqlInstancesExecuteSqlResponse, error) {
	in := rec.Instance
	req, _ := json.Marshal(proxy.QueryRequest{
		User: user, Database: orDefault(p.Database, "postgres"), SQL: p.SqlStatement,
		RowLimit: p.RowLimit, MaxBytes: dataAPIMaxResponse, AllowPartial: p.PartialResultMode == "ALLOW_PARTIAL_RESULT",
	})
	rt, _, err := s.plane(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, dataAPITimeout+5*time.Second)
	defer cancel()
	start := time.Now()
	cmd := []string{"env", agent.EnvVar + "=" + proxy.QueryName, proxy.EnvQueryTimeout + "=" + dataAPITimeout.String(), agent.ContainerPath}
	res, err := rt.Exec(ctx, containerName(rt, in.Project, in.Name), cmd, req)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			return nil, errNotRunning()
		}
		return nil, apierr.Internal("running the statements: %v", err)
	}
	if res.ExitCode != 0 {
		return nil, apierr.Internal("running the statements: %s", strings.TrimSpace(string(res.Stderr)))
	}
	var out sqladmin.SqlInstancesExecuteSqlResponse
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return nil, apierr.Internal("reading the result: %v", err)
	}
	out.Metadata = &sqladmin.Metadata{SqlStatementExecutionTime: seconds(time.Since(start))}
	return &out, nil
}

// seconds formats a google.protobuf.Duration as JSON ("1.250s").
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}
