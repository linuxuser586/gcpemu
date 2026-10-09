package iam

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/env"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// IAM Conditions (FR-IAM-005) are CEL expressions evaluated with cel-go,
// restricted to the attributes and functions IAM supports:
//
//	request.time  resource.name  resource.type  resource.service
//	&& || ! == != < <= > >= + - in, list literals
//	string: startsWith endsWith contains matches extract
//	timestamp() duration() and the timestamp get* methods (optional time zone)
//	resource.matchTag matchTagId hasTagKey hasTagKeyId
//	api.getAttribute(name, default)
//
// Macros (has, all, exists, …) and other standard functions are not
// available, so expressions using them are rejected by setIamPolicy.
// Workload identity attribute mappings and conditions use full CEL over
// assertion, attribute and google.

// condFunctions are the standard-library functions IAM Conditions allow.
var condFunctions = []string{
	operators.LogicalAnd, operators.LogicalOr, operators.LogicalNot,
	operators.Equals, operators.NotEquals,
	operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals,
	operators.Add, operators.Subtract, operators.In,
	overloads.StartsWith, overloads.EndsWith, overloads.Contains, overloads.Matches,
	overloads.TypeConvertTimestamp, overloads.TypeConvertDuration,
	overloads.TimeGetFullYear, overloads.TimeGetMonth, overloads.TimeGetDayOfYear,
	overloads.TimeGetDate, overloads.TimeGetDayOfMonth, overloads.TimeGetDayOfWeek,
	overloads.TimeGetHours, overloads.TimeGetMinutes, overloads.TimeGetSeconds,
	overloads.TimeGetMilliseconds,
}

var (
	condEnvOnce sync.Once
	condCELEnv  *cel.Env
	condEnvErr  error

	mapEnvOnce sync.Once
	mapCELEnv  *cel.Env
	mapEnvErr  error

	condPrograms sync.Map // expression → cel.Program
	mapPrograms  sync.Map
)

// extractFunc is IAM's string.extract(template): the text between the
// literal prefix and suffix around the template's single {placeholder},
// or "" when they do not match.
var extractFunc = cel.Function("extract",
	cel.MemberOverload("string_extract_string", []*cel.Type{cel.StringType, cel.StringType}, cel.StringType,
		cel.BinaryBinding(func(s, tmpl ref.Val) ref.Val {
			return types.String(extract(string(s.(types.String)), string(tmpl.(types.String))))
		})))

func extract(s, tmpl string) string {
	open, close := strings.IndexByte(tmpl, '{'), strings.IndexByte(tmpl, '}')
	if open < 0 || close < open {
		return ""
	}
	prefix, suffix := tmpl[:open], tmpl[close+1:]
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	if suffix == "" {
		return rest
	}
	if j := strings.Index(rest, suffix); j >= 0 {
		return rest[:j]
	}
	return ""
}

// tagFunc declares a resource tag function. The emulator does not model
// tags, so every tag test is false.
func tagFunc(name string, args int) cel.EnvOption {
	argTypes := make([]*cel.Type, args)
	for i := range argTypes {
		argTypes[i] = cel.StringType
	}
	return cel.Function("resource."+name,
		cel.Overload("resource_"+name, argTypes, cel.BoolType,
			cel.FunctionBinding(func(...ref.Val) ref.Val { return types.False })))
}

func conditionEnv() (*cel.Env, error) {
	condEnvOnce.Do(func() {
		subset := env.NewLibrarySubset().SetDisableMacros(true)
		for _, fn := range condFunctions {
			subset.AddIncludedFunctions(&env.Function{Name: fn})
		}
		condCELEnv, condEnvErr = cel.NewCustomEnv(
			cel.StdLib(cel.StdLibSubset(subset)),
			cel.Variable("request.time", cel.TimestampType),
			cel.Variable("resource.name", cel.StringType),
			cel.Variable("resource.type", cel.StringType),
			cel.Variable("resource.service", cel.StringType),
			extractFunc,
			tagFunc("matchTag", 2), tagFunc("matchTagId", 2),
			tagFunc("hasTagKey", 1), tagFunc("hasTagKeyId", 1),
			// No API attributes are modeled; the default is returned.
			cel.Function("api.getAttribute",
				cel.Overload("api_getAttribute", []*cel.Type{cel.StringType, cel.DynType}, cel.DynType,
					cel.BinaryBinding(func(_, def ref.Val) ref.Val { return def }))),
		)
	})
	return condCELEnv, condEnvErr
}

func mappingEnv() (*cel.Env, error) {
	mapEnvOnce.Do(func() {
		dynMap := cel.MapType(cel.StringType, cel.DynType)
		mapCELEnv, mapEnvErr = cel.NewEnv(
			cel.Variable("assertion", dynMap),
			cel.Variable("attribute", dynMap),
			cel.Variable("google", dynMap),
			extractFunc,
		)
	})
	return mapCELEnv, mapEnvErr
}

// compile compiles expr in e, caching the program in cache. When
// boolean is set the expression must evaluate to a bool.
func compile(e *cel.Env, envErr error, cache *sync.Map, expr string, boolean bool) (cel.Program, error) {
	if envErr != nil {
		return nil, envErr
	}
	if p, ok := cache.Load(expr); ok {
		return p.(cel.Program), nil
	}
	ast, iss := e.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if boolean && ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("expression must be boolean, got %s", ast.OutputType())
	}
	p, err := e.Program(ast)
	if err != nil {
		return nil, err
	}
	cache.Store(expr, p)
	return p, nil
}

// compileCondition compiles an IAM Condition.
func compileCondition(expr string) (cel.Program, error) {
	e, err := conditionEnv()
	return compile(e, err, &condPrograms, expr, true)
}

// compileMapping compiles a workload identity attribute mapping or
// attribute condition.
func compileMapping(expr string) (cel.Program, error) {
	e, err := mappingEnv()
	return compile(e, err, &mapPrograms, expr, false)
}

// evalMapping evaluates a workload identity expression over vars
// (assertion, attribute, google), returning a native Go value.
func evalMapping(expr string, vars map[string]any) (any, error) {
	p, err := compileMapping(expr)
	if err != nil {
		return nil, err
	}
	act := map[string]any{"assertion": map[string]any{}, "attribute": map[string]any{}, "google": map[string]any{}}
	for k, v := range vars {
		act[k] = v
	}
	v, _, err := p.Eval(act)
	if err != nil {
		return nil, err
	}
	return v.Value(), nil
}

// resourceType derives a resource.type value such as
// "storage.googleapis.com/Bucket" from a full resource name.
func resourceType(host, path string) string {
	segs := strings.Split(path, "/")
	coll := ""
	for i := 0; i+1 < len(segs); i += 2 {
		coll = segs[i]
		if coll == "objects" { // object names may contain slashes
			break
		}
	}
	switch {
	case strings.HasSuffix(coll, "ies"):
		coll = strings.TrimSuffix(coll, "ies") + "y"
	case strings.HasSuffix(coll, "sses"):
		coll = strings.TrimSuffix(coll, "es")
	default:
		coll = strings.TrimSuffix(coll, "s")
	}
	if coll == "" {
		return host
	}
	if coll == "version" && host == "secretmanager.googleapis.com" {
		coll = "secretVersion"
	}
	return host + "/" + strings.ToUpper(coll[:1]) + coll[1:]
}

// validateCondition compiles a condition expression.
func validateCondition(c *iamv1.Expr) error {
	if strings.TrimSpace(c.Expression) == "" {
		return apierr.InvalidArgument("Condition expression must not be empty.")
	}
	if c.Title == "" {
		return apierr.InvalidArgument("Condition title must be specified.")
	}
	if _, err := compileCondition(c.Expression); err != nil {
		return apierr.InvalidArgument("Condition expression %q is invalid: %v", c.Expression, err).WithReason(iamDomain, "INVALID_CONDITION")
	}
	return nil
}

// evalCondition evaluates c for an access to resource at now; compile or
// evaluation errors evaluate to false (deny).
func evalCondition(c *iamv1.Expr, resource string, now time.Time) bool {
	p, err := compileCondition(c.Expression)
	if err != nil {
		return false
	}
	host, path := splitResource(resource)
	v, _, err := p.Eval(map[string]any{
		"request.time":     now,
		"resource.name":    path,
		"resource.service": host,
		"resource.type":    resourceType(host, path),
	})
	return err == nil && v == types.True
}
