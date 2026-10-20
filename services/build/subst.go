package build

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// Substitutions (FR-CB-007): built-in and user $KEY / ${KEY} variables
// expanded in a Build's fields when it is created, with Cloud Build's
// checks. The parsing and the error texts follow Google's public
// cloud-build-local (subst/subst.go and validate/validate.go); the key
// rules and limits follow the "Substituting variable values" page.

// Limits of the substitutions map, as GCP documents them.
const (
	maxSubstitutions    = 200
	maxSubstKeyLength   = 100
	maxSubstValueLength = 4000
)

var (
	// userKey is the form of a user substitution's key.
	userKey = regexp.MustCompile(`^_[A-Z0-9_]+$`)
	// keyPrefix matches a substitution key at the start of a string.
	keyPrefix = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*`)
)

// builtinKeys are the built-in substitutions; a true one may be given a
// value in substitutions, as gcloud --substitutions does for the
// source's.
var builtinKeys = map[string]bool{
	"PROJECT_ID":                false,
	"PROJECT_NUMBER":            false,
	"BUILD_ID":                  false,
	"LOCATION":                  false,
	"SERVICE_ACCOUNT_EMAIL":     false,
	"SERVICE_ACCOUNT":           false,
	"TRIGGER_NAME":              false,
	"TRIGGER_BUILD_CONFIG_PATH": false,
	"REPO_FULL_NAME":            false,
	"REF_NAME":                  false,
	"REPO_NAME":                 true,
	"BRANCH_NAME":               true,
	"TAG_NAME":                  true,
	"REVISION_ID":               true,
	"COMMIT_SHA":                true,
	"SHORT_SHA":                 true,
}

// builtins returns a Build's built-in substitutions: its own, and those
// its source or Build trigger gives in source (COMMIT_SHA, BRANCH_NAME,
// TRIGGER_NAME, ...). Any other built-in is empty.
func builtins(b *cloudbuildpb.Build, location string, source map[string]string) map[string]string {
	vars := map[string]string{}
	for k := range builtinKeys {
		vars[k] = ""
	}
	for k, v := range source {
		if _, ok := builtinKeys[k]; ok {
			vars[k] = v
		}
	}
	vars["PROJECT_ID"] = b.GetProjectId()
	vars["PROJECT_NUMBER"] = project.NumberString(b.GetProjectId())
	vars["BUILD_ID"] = b.GetId()
	vars["LOCATION"] = location
	vars["SERVICE_ACCOUNT"] = b.GetServiceAccount()
	vars["SERVICE_ACCOUNT_EMAIL"], _ = serviceAccountEmail(b.GetServiceAccount())
	if vars["SHORT_SHA"] == "" {
		vars["SHORT_SHA"] = vars["COMMIT_SHA"][:min(7, len(vars["COMMIT_SHA"]))]
	}
	return vars
}

// substitute expands the substitutions in b's fields, in place: the
// user's in b.substitutions over the built-ins in vars. Under
// options.substitutionOption MUST_MATCH, a user substitution the fields
// do not use, or a $_KEY they use with no value, is INVALID_ARGUMENT;
// a key that is neither a user substitution nor a built-in always is.
// With options.dynamicSubstitutions, ${KEY...} takes bash's parameter
// expansions, and user values may refer to other substitutions.
func substitute(b *cloudbuildpb.Build, vars map[string]string) error {
	if err := checkSubstitutions(b.GetSubstitutions()); err != nil {
		return err
	}
	dynamic := b.GetOptions().GetDynamicSubstitutions()
	fields := substitutedFields(b)

	used := map[string]bool{}
	var warnings []string
	for _, f := range fields {
		var bad string
		walk(*f, dynamic, func(key string) {
			if _, ok := b.GetSubstitutions()[key]; ok {
				used[key] = true
				return
			}
			if userKey.MatchString(key) {
				warnings = append(warnings, fmt.Sprintf("key in the template %q is not matched in the substitution data; substitutions = %+v", key, b.GetSubstitutions()))
				return
			}
			if _, ok := builtinKeys[key]; !ok && bad == "" {
				bad = key
			}
		})
		if bad != "" {
			return apierr.InvalidArgument("invalid build: key in the template %q is not a valid built-in substitution", bad)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(b.GetSubstitutions())) {
		if !used[k] {
			warnings = append(warnings, fmt.Sprintf("key %q in the substitution data is not matched in the template", k))
		}
	}
	if len(warnings) > 0 && b.GetOptions().GetSubstitutionOption() != cloudbuildpb.BuildOptions_ALLOW_LOOSE {
		return apierr.InvalidArgument("invalid build: %s", strings.Join(warnings, ";"))
	}

	values, err := resolve(vars, b.GetSubstitutions(), dynamic)
	if err != nil {
		return err
	}
	for _, f := range fields {
		*f = expand(*f, dynamic, values)
	}
	return nil
}

// checkSubstitutions refuses a substitutions map that breaks GCP's
// limits, or names a key that is neither a user one nor an overridable
// built-in.
func checkSubstitutions(subs map[string]string) error {
	const prefix = "invalid build: invalid .substitutions field: "
	if len(subs) > maxSubstitutions {
		return apierr.InvalidArgument(prefix+"number of substitutions %d exceeded (max: %d)", len(subs), maxSubstitutions)
	}
	for _, k := range slices.Sorted(maps.Keys(subs)) {
		if len(k) > maxSubstKeyLength {
			return apierr.InvalidArgument(prefix+"substitution key %q too long (max: %d)", k, maxSubstKeyLength)
		}
		if !userKey.MatchString(k) && !builtinKeys[k] {
			return apierr.InvalidArgument(prefix+"substitution key %q does not respect format %q and is not an overridable built-in substitutions", k, userKey.String())
		}
		if len(subs[k]) > maxSubstValueLength {
			return apierr.InvalidArgument(prefix+"substitution value %q too long (max: %d)", subs[k], maxSubstValueLength)
		}
	}
	return nil
}

// substitutedFields returns the fields of b that substitutions apply to:
// each step's name, args, env, dir and entrypoint, images, tags,
// logsBucket, artifacts' images and objects, and the versionName of each
// availableSecrets.secretManager. A step's script is not among them:
// GCP's scripts take substitutions only as environment variables.
func substitutedFields(b *cloudbuildpb.Build) []*string {
	var fs []*string
	for _, st := range b.GetSteps() {
		fs = append(fs, &st.Name, &st.Dir, &st.Entrypoint)
		for i := range st.Args {
			fs = append(fs, &st.Args[i])
		}
		for i := range st.Env {
			fs = append(fs, &st.Env[i])
		}
	}
	for i := range b.Images {
		fs = append(fs, &b.Images[i])
	}
	for i := range b.Tags {
		fs = append(fs, &b.Tags[i])
	}
	fs = append(fs, &b.LogsBucket)
	if a := b.GetArtifacts(); a != nil {
		for i := range a.Images {
			fs = append(fs, &a.Images[i])
		}
		if o := a.GetObjects(); o != nil {
			fs = append(fs, &o.Location)
			for i := range o.Paths {
				fs = append(fs, &o.Paths[i])
			}
		}
	}
	for _, sec := range b.GetAvailableSecrets().GetSecretManager() {
		fs = append(fs, &sec.VersionName)
	}
	return fs
}

// resolve returns the value of every substitution: the built-ins in vars
// and the user's subs over them. With dynamic, a user value is expanded
// first, so that it may refer to other substitutions.
func resolve(vars, subs map[string]string, dynamic bool) (map[string]string, error) {
	out := maps.Clone(vars)
	maps.Copy(out, subs)
	if !dynamic {
		return out, nil
	}
	done := map[string]bool{}
	visiting := map[string]bool{}
	var err error
	var value func(key string) string
	value = func(key string) string {
		raw, user := subs[key]
		if !user || done[key] {
			return out[key]
		}
		if visiting[key] {
			if err == nil {
				err = apierr.InvalidArgument("invalid build: substitution %q refers to itself.", key)
			}
			return ""
		}
		visiting[key] = true
		out[key] = expandWith(raw, true, value)
		visiting[key], done[key] = false, true
		return out[key]
	}
	for _, k := range slices.Sorted(maps.Keys(subs)) {
		value(k)
	}
	return out, err
}

// param is a substitution expression in a string: in[start:end].
type param struct {
	start, end int
	// escape marks $$, a literal $.
	escape bool
	key    string
	// op is what follows the key in ${KEY...} with dynamic substitutions,
	// such as ":0:7"; length marks ${#KEY}.
	op     string
	length bool
}

// params finds the substitution expressions in s, as Cloud Build does:
// $$, $KEY and ${KEY} for KEY of [A-Z_][A-Z0-9_]*, and with dynamic
// ${KEY<op>} and ${#KEY}. A $ starting none of these is literal.
func params(s string, dynamic bool) []param {
	var ps []param
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			ps = append(ps, param{start: i, end: i + 2, escape: true})
			i++
			continue
		}
		if p, ok := parseParam(s, i, dynamic); ok {
			ps = append(ps, p)
			i = p.end - 1
		}
	}
	return ps
}

// parseParam parses the expression at s[i], a $.
func parseParam(s string, i int, dynamic bool) (param, bool) {
	rest := s[i+1:]
	if !strings.HasPrefix(rest, "{") {
		key := keyPrefix.FindString(rest)
		return param{start: i, end: i + 1 + len(key), key: key}, key != ""
	}
	end := closingBrace(s, i+1)
	if end < 0 {
		return param{}, false
	}
	inner := s[i+2 : end]
	p := param{start: i, end: end + 1}
	if dynamic && strings.HasPrefix(inner, "#") && len(inner) > 1 {
		p.length, inner = true, inner[1:]
	}
	p.key = keyPrefix.FindString(inner)
	p.op = inner[len(p.key):]
	switch {
	case p.key == "":
		return param{}, false
	case !dynamic || p.length:
		return p, p.op == ""
	}
	return p, true
}

// closingBrace returns the index of the } closing the { at s[open],
// skipping nested ${...}, or -1.
func closingBrace(s string, open int) int {
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// walk calls fn with each key s refers to, including those in the words
// of its expansions.
func walk(s string, dynamic bool, fn func(key string)) {
	for _, p := range params(s, dynamic) {
		if p.escape {
			continue
		}
		fn(p.key)
		walk(p.op, dynamic, fn)
	}
}

// expand replaces the expressions in s with their values.
func expand(s string, dynamic bool, values map[string]string) string {
	return expandWith(s, dynamic, func(key string) string { return values[key] })
}

func expandWith(s string, dynamic bool, value func(string) string) string {
	var out strings.Builder
	last := 0
	for _, p := range params(s, dynamic) {
		out.WriteString(s[last:p.start])
		last = p.end
		switch {
		case p.escape:
			out.WriteByte('$')
		case p.length:
			out.WriteString(strconv.Itoa(len(value(p.key))))
		default:
			v, ok := applyOp(value(p.key), p.op, func(w string) string { return expandWith(w, dynamic, value) })
			if !ok {
				v = s[p.start:p.end]
			}
			out.WriteString(v)
		}
	}
	out.WriteString(s[last:])
	return out.String()
}

// applyOp applies a bash parameter expansion to v: substrings (:N, :N:L),
// defaults (:-, -, :+, +), prefix and suffix removal (#, ##, %, %%),
// replacement (/, //, /#, /%) and case (^, ^^, ",", ",,"). word expands
// the words in op. An op it does not know reports false.
func applyOp(v, op string, word func(string) string) (string, bool) {
	switch {
	case op == "":
		return v, true
	case strings.HasPrefix(op, ":-"), strings.HasPrefix(op, ":="):
		if v == "" {
			return word(op[2:]), true
		}
		return v, true
	case strings.HasPrefix(op, ":+"):
		if v != "" {
			return word(op[2:]), true
		}
		return "", true
	case strings.HasPrefix(op, "-"), strings.HasPrefix(op, "="):
		// Every substitution is set, if only to empty.
		return v, true
	case strings.HasPrefix(op, "+"):
		return word(op[1:]), true
	case strings.HasPrefix(op, ":"):
		return substring(v, op[1:])
	case strings.HasPrefix(op, "##"):
		return trimPrefix(v, word(op[2:]), true), true
	case strings.HasPrefix(op, "#"):
		return trimPrefix(v, word(op[1:]), false), true
	case strings.HasPrefix(op, "%%"):
		return trimSuffix(v, word(op[2:]), true), true
	case strings.HasPrefix(op, "%"):
		return trimSuffix(v, word(op[1:]), false), true
	case strings.HasPrefix(op, "/"):
		return replace(v, op[1:], word), true
	case op == "^^":
		return strings.ToUpper(v), true
	case op == ",,":
		return strings.ToLower(v), true
	case op == "^":
		return strings.ToUpper(v[:min(1, len(v))]) + v[min(1, len(v)):], true
	case op == ",":
		return strings.ToLower(v[:min(1, len(v))]) + v[min(1, len(v)):], true
	}
	return "", false
}

// substring applies ${KEY:offset} or ${KEY:offset:length}, where a
// negative offset counts from the end and a negative length is an end
// offset from the end.
func substring(v, spec string) (string, bool) {
	offS, lenS, hasLen := strings.Cut(spec, ":")
	off, err := strconv.Atoi(strings.Trim(strings.TrimSpace(offS), "()"))
	if err != nil {
		return "", false
	}
	if off < 0 {
		off = max(0, len(v)+off)
	}
	off = min(off, len(v))
	end := len(v)
	if hasLen {
		n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(lenS), "()"))
		if err != nil {
			return "", false
		}
		if n < 0 {
			end = len(v) + n
		} else {
			end = off + n
		}
		end = min(max(end, off), len(v))
	}
	return v[off:end], true
}

// trimPrefix removes the shortest (or longest) prefix of v matching the
// glob pattern.
func trimPrefix(v, pattern string, longest bool) string {
	re := glob(pattern)
	for n := range len(v) + 1 {
		if longest {
			n = len(v) - n
		}
		if re.MatchString(v[:n]) {
			return v[n:]
		}
	}
	return v
}

// trimSuffix removes the shortest (or longest) suffix of v matching the
// glob pattern.
func trimSuffix(v, pattern string, longest bool) string {
	re := glob(pattern)
	for n := range len(v) + 1 {
		at := len(v) - n
		if longest {
			at = n
		}
		if re.MatchString(v[at:]) {
			return v[:at]
		}
	}
	return v
}

// replace applies ${KEY/pattern/string}: the longest match of the first
// occurrence, of every one with a leading /, of the one at the start
// with # and of the one at the end with %.
func replace(v, spec string, word func(string) string) string {
	var mode byte
	if spec != "" && strings.IndexByte("/#%", spec[0]) >= 0 {
		mode, spec = spec[0], spec[1:]
	}
	pattern, with, _ := cutUnescaped(spec, '/')
	pattern, with = word(pattern), word(with)
	if pattern == "" {
		return v
	}
	re := glob(pattern)
	// match returns the end of the longest match at i, or -1.
	match := func(i int) int {
		for j := len(v); j > i; j-- {
			if re.MatchString(v[i:j]) {
				return j
			}
			if mode == '%' {
				break
			}
		}
		return -1
	}
	var out strings.Builder
	i := 0
	for i < len(v) && (mode != '#' || i == 0) {
		end := match(i)
		if end < 0 {
			out.WriteByte(v[i])
			i++
			continue
		}
		out.WriteString(with)
		i = end
		if mode != '/' {
			break
		}
	}
	out.WriteString(v[i:])
	return out.String()
}

// cutUnescaped cuts s around the first sep not escaped by a backslash.
func cutUnescaped(s string, sep byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case sep:
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// glob compiles a bash glob pattern (*, ?, [...] and \ escapes) to an
// anchored regular expression.
func glob(pattern string) *regexp.Regexp {
	var re strings.Builder
	re.WriteString(`^(?s:`)
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			re.WriteString(`.*`)
		case '?':
			re.WriteString(`.`)
		case '\\':
			if i+1 < len(pattern) {
				i++
				re.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			} else {
				re.WriteString(`\\`)
			}
		case '[':
			if end := strings.IndexByte(pattern[i+1:], ']'); end > 0 {
				class := pattern[i+1 : i+1+end]
				if class[0] == '!' {
					class = "^" + class[1:]
				}
				re.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
				i += end + 1
				continue
			}
			re.WriteString(`\[`)
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString(`)$`)
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return regexp.MustCompile(`^` + regexp.QuoteMeta(pattern) + `$`)
	}
	return compiled
}
