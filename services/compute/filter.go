package compute

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Compute list filters (FR-CORE-024): comparisons "field OP value" with OP
// one of =, !=, : (string, '*' wildcards), eq, ne (RE2 full match),
// combined with AND, OR, NOT, implicit AND and parentheses, e.g.
// `(network = "https://...") (destRange = "0.0.0.0/0")` or `name eq web-.*`.

type filterExpr interface {
	match(item map[string]any) bool
}

type andExpr []filterExpr
type orExpr []filterExpr
type notExpr struct{ e filterExpr }
type cmpExpr struct {
	field, op, value string
	re               *regexp.Regexp
}

func (a andExpr) match(m map[string]any) bool {
	for _, e := range a {
		if !e.match(m) {
			return false
		}
	}
	return true
}

func (o orExpr) match(m map[string]any) bool {
	for _, e := range o {
		if e.match(m) {
			return true
		}
	}
	return false
}

func (n notExpr) match(m map[string]any) bool { return !n.e.match(m) }

func (c cmpExpr) match(m map[string]any) bool {
	vals := fieldValues(m, strings.Split(c.field, "."))
	any := false
	for _, v := range vals {
		if c.matchOne(v) {
			any = true
			break
		}
	}
	if c.op == "!=" || c.op == "ne" {
		// Negated operators hold when no value matches the positive form.
		return !any
	}
	return any
}

func (c cmpExpr) matchOne(v string) bool {
	switch c.op {
	case "eq", "ne":
		return c.re.MatchString(v)
	default: // =, !=, :
		if c.value == v {
			return true
		}
		if strings.Contains(v, "projects/") && strings.Contains(c.value, "projects/") && relPath(v) == relPath(c.value) {
			return true
		}
		if strings.Contains(c.value, "*") {
			ok, _ := path.Match(c.value, v)
			return ok
		}
		return false
	}
}

// fieldValues returns the string forms of a (possibly nested, possibly
// repeated) field.
func fieldValues(v any, segs []string) []string {
	if len(segs) == 0 {
		switch x := v.(type) {
		case nil:
			return nil
		case string:
			return []string{x}
		case bool:
			return []string{strconv.FormatBool(x)}
		case float64:
			return []string{strconv.FormatFloat(x, 'f', -1, 64)}
		case []any:
			var out []string
			for _, e := range x {
				out = append(out, fieldValues(e, nil)...)
			}
			return out
		default:
			return []string{fmt.Sprint(x)}
		}
	}
	switch x := v.(type) {
	case map[string]any:
		return fieldValues(x[segs[0]], segs[1:])
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fieldValues(e, segs)...)
		}
		return out
	}
	return nil
}

// parseFilter parses a compute filter expression; an empty one matches all.
func parseFilter(s string) (filterExpr, error) {
	toks, err := tokenize(s)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, nil
	}
	p := &filterParser{toks: toks}
	e, err := p.or()
	if err != nil {
		return nil, badFilter(s)
	}
	if p.i != len(p.toks) {
		return nil, badFilter(s)
	}
	return e, nil
}

func badFilter(s string) error {
	return apierr.InvalidArgument("Invalid value for field 'filter': '%s'. Invalid list filter expression.", s).WithLegacy("invalid")
}

type token struct {
	s      string
	quoted bool
}

func tokenize(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')' || c == ':' || c == '=':
			out = append(out, token{s: string(c)})
			i++
		case c == '!' && i+1 < len(s) && s[i+1] == '=':
			out = append(out, token{s: "!="})
			i += 2
		case c == '"' || c == '\'':
			j := i + 1
			var b strings.Builder
			for ; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, badFilter(s)
			}
			out = append(out, token{s: b.String(), quoted: true})
			i = j + 1
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n()=:!\"'", rune(s[j])) {
				j++
			}
			if j == i {
				return nil, badFilter(s)
			}
			out = append(out, token{s: s[i:j]})
			i = j
		}
	}
	return out, nil
}

type filterParser struct {
	toks []token
	i    int
}

func (p *filterParser) peek() (token, bool) {
	if p.i < len(p.toks) {
		return p.toks[p.i], true
	}
	return token{}, false
}

func (p *filterParser) or() (filterExpr, error) {
	first, err := p.and()
	if err != nil {
		return nil, err
	}
	out := orExpr{first}
	for {
		t, ok := p.peek()
		if !ok || t.quoted || !strings.EqualFold(t.s, "OR") {
			break
		}
		p.i++
		e, err := p.and()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if len(out) == 1 {
		return first, nil
	}
	return out, nil
}

func (p *filterParser) and() (filterExpr, error) {
	var out andExpr
	for {
		t, ok := p.peek()
		if !ok || (!t.quoted && (t.s == ")" || strings.EqualFold(t.s, "OR"))) {
			break
		}
		if !t.quoted && strings.EqualFold(t.s, "AND") {
			p.i++
			continue
		}
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return out, nil
}

func (p *filterParser) unary() (filterExpr, error) {
	t, _ := p.peek()
	if !t.quoted && (strings.EqualFold(t.s, "NOT") || t.s == "-") {
		p.i++
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notExpr{e}, nil
	}
	if !t.quoted && t.s == "(" {
		p.i++
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if t, ok := p.peek(); !ok || t.s != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.i++
		return e, nil
	}
	return p.cmp()
}

func (p *filterParser) cmp() (filterExpr, error) {
	if p.i+1 >= len(p.toks) {
		return nil, fmt.Errorf("incomplete comparison")
	}
	field := p.toks[p.i]
	op := p.toks[p.i+1]
	if op.quoted {
		return nil, fmt.Errorf("bad operator")
	}
	opS := strings.ToLower(op.s)
	switch opS {
	case "=", "!=", ":", "eq", "ne":
	default:
		return nil, fmt.Errorf("bad operator %q", op.s)
	}
	if p.i+2 >= len(p.toks) {
		return nil, fmt.Errorf("missing value")
	}
	val := p.toks[p.i+2]
	p.i += 3
	c := cmpExpr{field: field.s, op: opS, value: val.s}
	if opS == "eq" || opS == "ne" {
		re, err := regexp.Compile("^(?:" + val.s + ")$")
		if err != nil {
			return nil, err
		}
		c.re = re
	}
	return c, nil
}
