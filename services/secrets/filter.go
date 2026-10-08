package secrets

import (
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// List filters: a conjunction of terms "field:value" (contains) and
// "field=value" (equals), where value "*" matches any present value and
// field may be labels.KEY or annotations.KEY. Terms are separated by
// spaces or AND; OR, NOT and comparisons answer INVALID_ARGUMENT.

// fieldFunc returns the values of field on m, and whether the field is
// known.
type fieldFunc func(m proto.Message, field string) ([]string, bool)

func secretField(m proto.Message, field string) ([]string, bool) {
	s := m.(*secretmanagerpb.Secret)
	if k, ok := strings.CutPrefix(field, "labels."); ok {
		v, has := s.GetLabels()[k]
		return present(v, has), true
	}
	if k, ok := strings.CutPrefix(field, "annotations."); ok {
		v, has := s.GetAnnotations()[k]
		return present(v, has), true
	}
	switch field {
	case "name":
		return []string{s.GetName()}, true
	case "labels":
		return keys(s.GetLabels()), true
	case "annotations":
		return keys(s.GetAnnotations()), true
	case "topics", "topics.name":
		var out []string
		for _, t := range s.GetTopics() {
			out = append(out, t.GetName())
		}
		return out, true
	case "secret_type", "secretType":
		return []string{s.GetSecretType().String()}, true
	}
	return nil, false
}

func versionField(m proto.Message, field string) ([]string, bool) {
	v := m.(*secretmanagerpb.SecretVersion)
	switch field {
	case "name":
		return []string{v.GetName()}, true
	case "state":
		return []string{v.GetState().String()}, true
	}
	return nil, false
}

func present(v string, ok bool) []string {
	if !ok {
		return nil
	}
	return []string{v}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// parseFilter compiles filter; zero is an empty message used to reject
// unknown fields.
func parseFilter(filter string, field fieldFunc, zero proto.Message) (func(proto.Message) bool, error) {
	type term struct {
		field, value string
		eq           bool
	}
	var terms []term
	for _, tok := range strings.Fields(filter) {
		if tok == "AND" {
			continue
		}
		if tok == "OR" || tok == "NOT" || strings.HasPrefix(tok, "-") || strings.ContainsAny(tok, "<>()") {
			return nil, apierr.InvalidArgument("Unsupported filter %q: the emulator supports AND of field:value and field=value terms.", filter)
		}
		i := strings.IndexAny(tok, ":=")
		if i <= 0 {
			return nil, apierr.InvalidArgument("Invalid filter term %q.", tok)
		}
		t := term{field: tok[:i], value: strings.Trim(tok[i+1:], `"`), eq: tok[i] == '='}
		if _, ok := field(zero, t.field); !ok {
			return nil, apierr.InvalidArgument("Unknown filter field %q.", t.field)
		}
		terms = append(terms, t)
	}
	return func(m proto.Message) bool {
		for _, t := range terms {
			vals, ok := field(m, t.field)
			if !ok {
				return false
			}
			hit := false
			for _, v := range vals {
				if t.value == "*" || (t.eq && v == t.value) || (!t.eq && strings.Contains(v, t.value)) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
		return true
	}, nil
}
