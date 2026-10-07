package pubsub

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Subscription filter language (FR-PS-005):
//
//	expr    = and { "OR" and } | and { "AND" ... }   (AND and OR may not be mixed without parentheses)
//	unary   = [ "NOT" | "-" ] primary
//	primary = "(" expr ")"
//	        | "attributes" ":" key                  (attribute exists)
//	        | "attributes" "." key ( "=" | "!=" ) string
//	        | "hasPrefix" "(" "attributes" "." key "," string ")"
//	key     = identifier | string
//
// Messages that do not match are acknowledged automatically on publish.

// maxFilterLen is the GCP limit on filter length in bytes.
const maxFilterLen = 256

// filterExpr is a compiled filter.
type filterExpr interface {
	match(attrs map[string]string) bool
}

type (
	orExpr  []filterExpr
	andExpr []filterExpr
	notExpr struct{ x filterExpr }
	hasExpr struct{ key string }
	eqExpr  struct {
		key, val string
		neg      bool
	}
	prefixExpr struct{ key, prefix string }
)

func (e orExpr) match(a map[string]string) bool {
	for _, x := range e {
		if x.match(a) {
			return true
		}
	}
	return false
}

func (e andExpr) match(a map[string]string) bool {
	for _, x := range e {
		if !x.match(a) {
			return false
		}
	}
	return true
}

func (e notExpr) match(a map[string]string) bool { return !e.x.match(a) }

func (e hasExpr) match(a map[string]string) bool { _, ok := a[e.key]; return ok }

func (e eqExpr) match(a map[string]string) bool {
	v, ok := a[e.key]
	if e.neg {
		return !ok || v != e.val
	}
	return ok && v == e.val
}

func (e prefixExpr) match(a map[string]string) bool {
	v, ok := a[e.key]
	return ok && strings.HasPrefix(v, e.prefix)
}

// compileFilter parses a filter; "" compiles to nil (match everything).
func compileFilter(src string) (filterExpr, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	if len(src) > maxFilterLen {
		return nil, fmt.Errorf("filter exceeds the maximum length of %d bytes", maxFilterLen)
	}
	toks, err := lexFilter(src)
	if err != nil {
		return nil, err
	}
	p := &filterParser{toks: toks}
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, fmt.Errorf("unexpected %q at position %d", t.text, t.pos)
	}
	return e, nil
}

// filterMatches reports whether attrs pass f (nil matches everything).
func filterMatches(f filterExpr, attrs map[string]string) bool {
	return f == nil || f.match(attrs)
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokString
	tokLParen
	tokRParen
	tokDot
	tokColon
	tokComma
	tokEq
	tokNe
	tokMinus
)

type token struct {
	kind tokKind
	text string // identifier text or decoded string value
	pos  int
}

func lexFilter(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.IsSpace(r):
			i += w
		case r == '(':
			out = append(out, token{tokLParen, "(", i})
			i++
		case r == ')':
			out = append(out, token{tokRParen, ")", i})
			i++
		case r == '.':
			out = append(out, token{tokDot, ".", i})
			i++
		case r == ':':
			out = append(out, token{tokColon, ":", i})
			i++
		case r == ',':
			out = append(out, token{tokComma, ",", i})
			i++
		case r == '=':
			out = append(out, token{tokEq, "=", i})
			i++
		case r == '-':
			out = append(out, token{tokMinus, "-", i})
			i++
		case r == '!':
			if i+1 < len(s) && s[i+1] == '=' {
				out = append(out, token{tokNe, "!=", i})
				i += 2
				continue
			}
			return nil, fmt.Errorf("unexpected '!' at position %d", i)
		case r == '"' || r == '\'':
			val, n, err := lexString(s[i:])
			if err != nil {
				return nil, fmt.Errorf("%v at position %d", err, i)
			}
			out = append(out, token{tokString, val, i})
			i += n
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			j := i
			for j < len(s) {
				r2, w2 := utf8.DecodeRuneInString(s[j:])
				if r2 != '_' && r2 != '-' && !unicode.IsLetter(r2) && !unicode.IsDigit(r2) {
					break
				}
				j += w2
			}
			out = append(out, token{tokIdent, s[i:j], i})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", r, i)
		}
	}
	return append(out, token{tokEOF, "end of filter", len(s)}), nil
}

// lexString decodes a quoted string with backslash escapes.
func lexString(s string) (string, int, error) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case q:
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(s) {
				return "", 0, fmt.Errorf("unterminated string")
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, fmt.Errorf("unterminated string")
}

type filterParser struct {
	toks []token
	i    int
}

func (p *filterParser) peek() token { return p.toks[p.i] }

func (p *filterParser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *filterParser) expect(k tokKind, what string) (token, error) {
	t := p.next()
	if t.kind != k {
		return t, fmt.Errorf("expected %s at position %d, got %q", what, t.pos, t.text)
	}
	return t, nil
}

func isKeyword(t token, kw string) bool { return t.kind == tokIdent && t.text == kw }

func (p *filterParser) expr() (filterExpr, error) {
	first, err := p.unary()
	if err != nil {
		return nil, err
	}
	var op string
	items := []filterExpr{first}
	for {
		t := p.peek()
		if !isKeyword(t, "AND") && !isKeyword(t, "OR") {
			break
		}
		if op != "" && t.text != op {
			return nil, fmt.Errorf("AND and OR cannot be mixed without parentheses (position %d)", t.pos)
		}
		op = t.text
		p.next()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		items = append(items, x)
	}
	switch op {
	case "AND":
		return andExpr(items), nil
	case "OR":
		return orExpr(items), nil
	}
	return first, nil
}

func (p *filterParser) unary() (filterExpr, error) {
	t := p.peek()
	if isKeyword(t, "NOT") || t.kind == tokMinus {
		p.next()
		x, err := p.primary()
		if err != nil {
			return nil, err
		}
		return notExpr{x}, nil
	}
	return p.primary()
}

func (p *filterParser) primary() (filterExpr, error) {
	t := p.next()
	switch {
	case t.kind == tokLParen:
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "')'"); err != nil {
			return nil, err
		}
		return x, nil
	case isKeyword(t, "hasPrefix"):
		if _, err := p.expect(tokLParen, "'('"); err != nil {
			return nil, err
		}
		if t2 := p.next(); !isKeyword(t2, "attributes") {
			return nil, fmt.Errorf("hasPrefix expects an attribute at position %d", t2.pos)
		}
		if _, err := p.expect(tokDot, "'.'"); err != nil {
			return nil, err
		}
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokComma, "','"); err != nil {
			return nil, err
		}
		pre, err := p.expect(tokString, "string literal")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "')'"); err != nil {
			return nil, err
		}
		return prefixExpr{key, pre.text}, nil
	case isKeyword(t, "attributes"):
		sep := p.next()
		switch sep.kind {
		case tokColon:
			key, err := p.key()
			if err != nil {
				return nil, err
			}
			return hasExpr{key}, nil
		case tokDot:
			key, err := p.key()
			if err != nil {
				return nil, err
			}
			op := p.next()
			if op.kind != tokEq && op.kind != tokNe {
				if op.kind == tokColon {
					return nil, fmt.Errorf("unsupported operator ':' after attributes.%s at position %d", key, op.pos)
				}
				return nil, fmt.Errorf("expected '=' or '!=' at position %d, got %q", op.pos, op.text)
			}
			v, err := p.expect(tokString, "string literal")
			if err != nil {
				return nil, err
			}
			return eqExpr{key: key, val: v.text, neg: op.kind == tokNe}, nil
		}
		return nil, fmt.Errorf("expected '.' or ':' after attributes at position %d", sep.pos)
	}
	return nil, fmt.Errorf("unexpected %q at position %d", t.text, t.pos)
}

func (p *filterParser) key() (string, error) {
	t := p.next()
	if t.kind == tokIdent || t.kind == tokString {
		return t.text, nil
	}
	return "", fmt.Errorf("expected attribute key at position %d, got %q", t.pos, t.text)
}
