package iam

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// IAM Conditions (FR-IAM-005). A small CEL subset is supported:
//
//	request.time  < <= > >= == !=  timestamp("RFC3339")
//	resource.name / resource.type / resource.service  == != and
//	  .startsWith("…") .endsWith("…") .contains("…")
//	&& || ! ( ) true false
//
// Unsupported expressions are rejected by setIamPolicy.

// condEnv is the evaluation context of a condition. vars holds extra
// roots (assertion, attribute, google) for workload identity mappings.
type condEnv struct {
	now      time.Time
	resource string // full resource name
	vars     map[string]any
}

// varRoots are the identifier roots resolved through condEnv.vars.
var varRoots = map[string]bool{"assertion": true, "attribute": true, "google": true}

func (e condEnv) attr(name string) (any, bool) {
	root, _, _ := strings.Cut(name, ".")
	if varRoots[root] {
		var cur any = e.vars
		for _, part := range strings.Split(name, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, true
			}
			cur = m[part]
		}
		return cur, true
	}
	host, path := splitResource(e.resource)
	switch name {
	case "request.time":
		return e.now, true
	case "resource.name":
		return path, true
	case "resource.service":
		return host, true
	case "resource.type":
		return resourceType(host, path), true
	}
	return nil, false
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
	return host + "/" + strings.ToUpper(coll[:1]) + coll[1:]
}

// validateCondition parses a condition expression.
func validateCondition(c *iamv1.Expr) error {
	if strings.TrimSpace(c.Expression) == "" {
		return apierr.InvalidArgument("Condition expression must not be empty.")
	}
	if c.Title == "" {
		return apierr.InvalidArgument("Condition title must be specified.")
	}
	if _, err := parseCond(c.Expression); err != nil {
		return apierr.InvalidArgument("Condition expression %q is invalid: %v", c.Expression, err).WithReason(iamDomain, "INVALID_CONDITION")
	}
	return nil
}

// evalCondition evaluates c for an access to resource at now; parse or
// type errors evaluate to false (deny).
func evalCondition(c *iamv1.Expr, resource string, now time.Time) bool {
	n, err := parseCond(c.Expression)
	if err != nil {
		return false
	}
	v, err := n.eval(condEnv{now: now, resource: resource})
	if err != nil {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// --- lexer ---

type tok struct {
	kind string // ident, string, op, (, ), ",", "."
	val  string
}

func lexCond(s string) ([]tok, error) {
	var out []tok
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '"' || c == '\'':
			j := i + 1
			var sb strings.Builder
			for j < len(s) && rune(s[j]) != c {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				sb.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, tok{"string", sb.String()})
			i = j + 1
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) || s[j] == '_') {
				j++
			}
			out = append(out, tok{"ident", s[i:j]})
			i = j
		case c == '(' || c == ')' || c == ',' || c == '.':
			out = append(out, tok{string(c), string(c)})
			i++
		default:
			for _, op := range []string{"&&", "||", "==", "!=", "<=", ">=", "<", ">", "!", "+"} {
				if strings.HasPrefix(s[i:], op) {
					out = append(out, tok{"op", op})
					i += len(op)
					goto next
				}
			}
			return nil, fmt.Errorf("unexpected character %q", c)
		next:
		}
	}
	return out, nil
}

// --- parser ---

type condNode interface {
	eval(e condEnv) (any, error)
}

type (
	litNode  struct{ v any }
	attrNode struct{ name string }
	notNode  struct{ x condNode }
	binNode  struct {
		op   string
		l, r condNode
	}
	callNode struct {
		recv condNode // nil for global functions
		fn   string
		args []condNode
	}
)

type condParser struct {
	toks []tok
	pos  int
}

func parseCond(s string) (condNode, error) {
	toks, err := lexCond(s)
	if err != nil {
		return nil, err
	}
	p := &condParser{toks: toks}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].val)
	}
	return n, nil
}

func (p *condParser) peek() (tok, bool) {
	if p.pos >= len(p.toks) {
		return tok{}, false
	}
	return p.toks[p.pos], true
}

func (p *condParser) accept(kind, val string) bool {
	t, ok := p.peek()
	if ok && t.kind == kind && (val == "" || t.val == val) {
		p.pos++
		return true
	}
	return false
}

func (p *condParser) or() (condNode, error) {
	l, err := p.and()
	for err == nil && p.accept("op", "||") {
		var r condNode
		if r, err = p.and(); err == nil {
			l = &binNode{"||", l, r}
		}
	}
	return l, err
}

func (p *condParser) and() (condNode, error) {
	l, err := p.cmp()
	for err == nil && p.accept("op", "&&") {
		var r condNode
		if r, err = p.cmp(); err == nil {
			l = &binNode{"&&", l, r}
		}
	}
	return l, err
}

var cmpOps = map[string]bool{"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *condParser) cmp() (condNode, error) {
	l, err := p.add()
	if err != nil {
		return nil, err
	}
	if t, ok := p.peek(); ok && t.kind == "op" && cmpOps[t.val] {
		p.pos++
		r, err := p.add()
		if err != nil {
			return nil, err
		}
		return &binNode{t.val, l, r}, nil
	}
	return l, nil
}

func (p *condParser) add() (condNode, error) {
	l, err := p.unary()
	for err == nil && p.accept("op", "+") {
		var r condNode
		if r, err = p.unary(); err == nil {
			l = &binNode{"+", l, r}
		}
	}
	return l, err
}

func (p *condParser) unary() (condNode, error) {
	if p.accept("op", "!") {
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &notNode{x}, nil
	}
	return p.postfix()
}

func (p *condParser) postfix() (condNode, error) {
	n, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.accept(".", "") {
		t, ok := p.peek()
		if !ok || t.kind != "ident" {
			return nil, fmt.Errorf("expected method name")
		}
		p.pos++
		if !p.accept("(", "") {
			return nil, fmt.Errorf("unsupported field access .%s", t.val)
		}
		args, err := p.args()
		if err != nil {
			return nil, err
		}
		n = &callNode{recv: n, fn: t.val, args: args}
	}
	return n, nil
}

func (p *condParser) args() ([]condNode, error) {
	var args []condNode
	if p.accept(")", "") {
		return nil, nil
	}
	for {
		a, err := p.or()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.accept(")", "") {
			return args, nil
		}
		if !p.accept(",", "") {
			return nil, fmt.Errorf("expected , or )")
		}
	}
}

func (p *condParser) primary() (condNode, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	p.pos++
	switch t.kind {
	case "string":
		return &litNode{t.val}, nil
	case "(":
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.accept(")", "") {
			return nil, fmt.Errorf("missing )")
		}
		return n, nil
	case "ident":
		switch t.val {
		case "true", "false":
			return &litNode{t.val == "true"}, nil
		case "timestamp", "duration":
			if !p.accept("(", "") {
				return nil, fmt.Errorf("expected ( after %s", t.val)
			}
			args, err := p.args()
			if err != nil {
				return nil, err
			}
			return &callNode{fn: t.val, args: args}, nil
		case "request", "resource":
			name := t.val
			if !p.accept(".", "") {
				return nil, fmt.Errorf("expected attribute after %s", t.val)
			}
			f, ok := p.peek()
			if !ok || f.kind != "ident" {
				return nil, fmt.Errorf("expected attribute name")
			}
			p.pos++
			name += "." + f.val
			if _, ok := (condEnv{}).attr(name); !ok {
				return nil, fmt.Errorf("unsupported attribute %s", name)
			}
			return &attrNode{name}, nil
		default:
			if !varRoots[t.val] {
				break
			}
			name := t.val
			// Consume ".field" segments, leaving ".method(" to postfix.
			for p.pos+1 < len(p.toks) && p.toks[p.pos].kind == "." && p.toks[p.pos+1].kind == "ident" &&
				(p.pos+2 >= len(p.toks) || p.toks[p.pos+2].kind != "(") {
				name += "." + p.toks[p.pos+1].val
				p.pos += 2
			}
			return &attrNode{name}, nil
		}
	}
	return nil, fmt.Errorf("unexpected %q", t.val)
}

// --- evaluation ---

func (n *litNode) eval(condEnv) (any, error) { return n.v, nil }

func (n *attrNode) eval(e condEnv) (any, error) {
	v, _ := e.attr(n.name)
	return v, nil
}

func (n *notNode) eval(e condEnv) (any, error) {
	v, err := n.x.eval(e)
	if err != nil {
		return nil, err
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("! on non-bool")
	}
	return !b, nil
}

func (n *binNode) eval(e condEnv) (any, error) {
	l, err := n.l.eval(e)
	if err != nil {
		return nil, err
	}
	if n.op == "&&" || n.op == "||" {
		lb, ok := l.(bool)
		if !ok {
			return nil, fmt.Errorf("%s on non-bool", n.op)
		}
		if (n.op == "&&" && !lb) || (n.op == "||" && lb) {
			return lb, nil
		}
		r, err := n.r.eval(e)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, fmt.Errorf("%s on non-bool", n.op)
		}
		return rb, nil
	}
	r, err := n.r.eval(e)
	if err != nil {
		return nil, err
	}
	if n.op == "+" {
		ls, lok := l.(string)
		rs, rok := r.(string)
		if !lok || !rok {
			return nil, fmt.Errorf("+ needs strings")
		}
		return ls + rs, nil
	}
	var c int
	switch lv := l.(type) {
	case time.Time:
		rv, ok := r.(time.Time)
		if !ok {
			return nil, fmt.Errorf("type mismatch")
		}
		c = lv.Compare(rv)
	case string:
		rv, ok := r.(string)
		if !ok {
			return nil, fmt.Errorf("type mismatch")
		}
		c = strings.Compare(lv, rv)
	case bool:
		rv, ok := r.(bool)
		if !ok || (n.op != "==" && n.op != "!=") {
			return nil, fmt.Errorf("type mismatch")
		}
		if lv != rv {
			c = 1
		}
	default:
		return nil, fmt.Errorf("unsupported operand")
	}
	switch n.op {
	case "==":
		return c == 0, nil
	case "!=":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return nil, fmt.Errorf("unknown operator %s", n.op)
}

func (n *callNode) eval(e condEnv) (any, error) {
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(e)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	if n.recv == nil {
		if len(args) != 1 {
			return nil, fmt.Errorf("%s takes one argument", n.fn)
		}
		str, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("%s takes a string", n.fn)
		}
		switch n.fn {
		case "timestamp":
			return time.Parse(time.RFC3339Nano, str)
		case "duration":
			if sec, ok := strings.CutSuffix(str, "s"); ok {
				f, err := strconv.ParseFloat(sec, 64)
				return time.Duration(f * float64(time.Second)), err
			}
			return time.ParseDuration(str)
		}
		return nil, fmt.Errorf("unknown function %s", n.fn)
	}
	recv, err := n.recv.eval(e)
	if err != nil {
		return nil, err
	}
	rs, ok := recv.(string)
	if !ok || len(args) != 1 {
		return nil, fmt.Errorf("unsupported method %s", n.fn)
	}
	arg, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("%s takes a string", n.fn)
	}
	switch n.fn {
	case "startsWith":
		return strings.HasPrefix(rs, arg), nil
	case "endsWith":
		return strings.HasSuffix(rs, arg), nil
	case "contains":
		return strings.Contains(rs, arg), nil
	}
	return nil, fmt.Errorf("unsupported method %s", n.fn)
}
