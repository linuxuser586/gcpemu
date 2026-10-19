// Package listfilter evaluates the filter of a list call, as gcloud and
// the client libraries send it: field OP value terms (value bare or
// double-quoted) combined with AND, OR, NOT and parentheses, adjacent
// terms ANDed. String fields take =, != and : (contains, or equality on
// an Exact field, which refuses AIP-160's :* key-exists test), and a
// many-valued field matches when any of its values does (!= when none
// does). A prefix field (settings.userLabels.) takes every longer field
// name, the rest of the name a key; a resource without the key reads as
// empty. Time fields take = != < <= > >= against RFC 3339 timestamps,
// and a resource without the time matches only !=. An unknown field, an
// operator a field does not take or a syntax error is the Service's
// INVALID_ARGUMENT.
package listfilter

import (
	"slices"
	"strings"
	"time"
)

// Field is a filterable field of T.
type Field[T any] struct {
	// Time compares the field as an RFC 3339 timestamp.
	Time bool
	// Exact makes : compare a string field for equality, like =.
	Exact bool
	// Get returns the field's value.
	Get func(T) string
	// Values, when set instead of Get, returns a many-valued field's
	// values (a Build's tags).
	Values func(T) []string
	// Key, when set instead of Get, makes the field a prefix field: its
	// name in the map is a prefix (settings.userLabels.) that every
	// longer field name matches, and Key gets the rest of that name (a
	// label key). A field name in the map is looked up before a prefix
	// field.
	Key func(v T, key string) string
}

// lookup finds the field called name and returns it with its values
// getter.
func lookup[T any](fields map[string]Field[T], name string) (Field[T], func(T) []string, bool) {
	f, ok := fields[name]
	if ok && f.Key == nil {
		if f.Values != nil {
			return f, f.Values, true
		}
		return f, func(v T) []string { return []string{f.Get(v)} }, true
	}
	prefix := ""
	for p, pf := range fields {
		if pf.Key != nil && len(name) > len(p) && strings.HasPrefix(name, p) && len(p) > len(prefix) {
			f, prefix = pf, p
		}
	}
	if prefix == "" {
		return f, nil, false
	}
	key := name[len(prefix):]
	return f, func(v T) []string { return []string{f.Key(v, key)} }, true
}

// Compile parses filter into a predicate over T; an empty filter matches
// everything. invalid builds the Service's INVALID_ARGUMENT error for a
// filter it refuses.
func Compile[T any](filter string, fields map[string]Field[T], invalid func(format string, args ...any) error) (func(T) bool, error) {
	toks, err := lex(filter, invalid)
	if err != nil {
		return nil, err
	}
	p := &parser[T]{toks: toks, fields: fields, filter: filter, invalid: invalid}
	if len(toks) == 0 {
		return func(T) bool { return true }, nil
	}
	m, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, p.errorf("unexpected %q", p.toks[p.pos].text)
	}
	return m, nil
}

type filterTokKind int

const (
	tokWord filterTokKind = iota
	tokString
	tokOp
	tokLParen
	tokRParen
)

type filterTok struct {
	kind filterTokKind
	text string
}

// lex splits filter into words, quoted strings, operators and
// parentheses.
func lex(filter string, invalid func(format string, args ...any) error) ([]filterTok, error) {
	var toks []filterTok
	for i := 0; i < len(filter); {
		c := filter[i]
		switch {
		case isFilterSpace(c):
			i++
		case c == '(':
			toks = append(toks, filterTok{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, filterTok{tokRParen, ")"})
			i++
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(filter) && filter[j] != '"'; j++ {
				if filter[j] == '\\' && j+1 < len(filter) {
					j++
				}
				b.WriteByte(filter[j])
			}
			if j >= len(filter) {
				return nil, invalid("Invalid filter %q: unterminated string.", filter)
			}
			toks = append(toks, filterTok{tokString, b.String()})
			i = j + 1
		case strings.ContainsRune("=!<>:", rune(c)):
			op := string(c)
			if i+1 < len(filter) && filter[i+1] == '=' && c != '=' && c != ':' {
				op += "="
			}
			if op == "!" {
				return nil, invalid("Invalid filter %q: unexpected \"!\".", filter)
			}
			toks = append(toks, filterTok{tokOp, op})
			i += len(op)
		default:
			j := i
			for j < len(filter) && !isFilterSpace(filter[j]) && !strings.ContainsRune(`()"=!<>:`, rune(filter[j])) {
				j++
			}
			toks = append(toks, filterTok{tokWord, filter[i:j]})
			i = j
		}
	}
	return toks, nil
}

// isFilterSpace reports whether c separates filter tokens. Only ASCII
// whitespace does: other bytes, even of Unicode spaces, are part of a
// word.
func isFilterSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

type parser[T any] struct {
	toks    []filterTok
	pos     int
	fields  map[string]Field[T]
	filter  string
	invalid func(format string, args ...any) error
}

func (p *parser[T]) errorf(format string, args ...any) error {
	return p.invalid("Invalid filter %q: "+format+".", append([]any{p.filter}, args...)...)
}

func (p *parser[T]) peekWord(w string) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == tokWord && p.toks[p.pos].text == w
}

// or := and { OR and }
func (p *parser[T]) or() (func(T) bool, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peekWord("OR") {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		a := l
		l = func(v T) bool { return a(v) || r(v) }
	}
	return l, nil
}

// and := unary { [AND] unary }
func (p *parser[T]) and() (func(T) bool, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.pos < len(p.toks) && p.toks[p.pos].kind != tokRParen && !p.peekWord("OR") {
		if p.peekWord("AND") {
			p.pos++
		}
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		a := l
		l = func(v T) bool { return a(v) && r(v) }
	}
	return l, nil
}

// unary := NOT unary | ( or ) | term
func (p *parser[T]) unary() (func(T) bool, error) {
	if p.pos >= len(p.toks) {
		return nil, p.errorf("unexpected end")
	}
	switch t := p.toks[p.pos]; {
	case t.kind == tokWord && t.text == "NOT":
		p.pos++
		m, err := p.unary()
		if err != nil {
			return nil, err
		}
		return func(v T) bool { return !m(v) }, nil
	case t.kind == tokLParen:
		p.pos++
		m, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.toks) || p.toks[p.pos].kind != tokRParen {
			return nil, p.errorf("missing \")\"")
		}
		p.pos++
		return m, nil
	}
	return p.term()
}

// term := field op value
func (p *parser[T]) term() (func(T) bool, error) {
	if p.pos+3 > len(p.toks) {
		return nil, p.errorf("expected a field, an operator and a value")
	}
	name, op, val := p.toks[p.pos], p.toks[p.pos+1], p.toks[p.pos+2]
	if name.kind != tokWord || op.kind != tokOp || (val.kind != tokWord && val.kind != tokString) {
		return nil, p.errorf("expected a field, an operator and a value at %q", name.text)
	}
	p.pos += 3
	f, values, ok := lookup(p.fields, name.text)
	if !ok {
		return nil, p.errorf("unknown field %q", name.text)
	}
	if !f.Time {
		eq := func(v T) bool { return slices.Contains(values(v), val.text) }
		switch op.text {
		case "=":
			return eq, nil
		case "!=":
			return func(v T) bool { return !eq(v) }, nil
		case ":":
			if !f.Exact {
				return func(v T) bool {
					return slices.ContainsFunc(values(v), func(s string) bool { return strings.Contains(s, val.text) })
				}, nil
			}
			if val.kind == tokWord && val.text == "*" {
				return nil, p.errorf("field %q does not support \":*\"", name.text)
			}
			return eq, nil
		}
		return nil, p.errorf("field %q does not support %q", name.text, op.text)
	}
	var cmp func(int) bool
	switch op.text {
	case "=":
		cmp = func(c int) bool { return c == 0 }
	case "!=":
		cmp = func(c int) bool { return c != 0 }
	case "<":
		cmp = func(c int) bool { return c < 0 }
	case "<=":
		cmp = func(c int) bool { return c <= 0 }
	case ">":
		cmp = func(c int) bool { return c > 0 }
	case ">=":
		cmp = func(c int) bool { return c >= 0 }
	default:
		return nil, p.errorf("field %q does not support %q", name.text, op.text)
	}
	want, err := time.Parse(time.RFC3339Nano, val.text)
	if err != nil {
		return nil, p.errorf("%q is not an RFC 3339 timestamp", val.text)
	}
	return func(v T) bool {
		got, err := time.Parse(time.RFC3339Nano, values(v)[0])
		if err != nil {
			return op.text == "!="
		}
		return cmp(got.Compare(want))
	}, nil
}
