package sql

import (
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// List filters, as gcloud's sql filter rewrite sends them to the server:
// field OP value terms (value bare or double-quoted) combined with AND,
// OR, NOT and parentheses, adjacent terms ANDed. String fields take =, !=
// and : (contains); time fields take = != < <= > >= against RFC 3339
// timestamps, and a resource without the time matches only !=. An
// unknown field, an operator a field does not take or a syntax error is
// INVALID_ARGUMENT.

// filterField is a filterable field of T.
type filterField[T any] struct {
	time bool
	get  func(T) string
}

// backupFilterFields are the backups.list filter fields.
var backupFilterFields = map[string]filterField[*sqladmin.Backup]{
	"instance": {get: func(b *sqladmin.Backup) string { return b.Instance }},
	"type":     {get: func(b *sqladmin.Backup) string { return b.Type }},
	"location": {get: func(b *sqladmin.Backup) string { return b.Location }},
	"backupInterval.startTime": {time: true, get: func(b *sqladmin.Backup) string {
		if b.BackupInterval == nil {
			return ""
		}
		return b.BackupInterval.StartTime
	}},
	"instanceDeletionTime": {time: true, get: func(b *sqladmin.Backup) string { return b.InstanceDeletionTime }},
}

// compileFilter parses filter into a predicate over T; an empty filter
// matches everything.
func compileFilter[T any](filter string, fields map[string]filterField[T]) (func(T) bool, error) {
	toks, err := lexFilter(filter)
	if err != nil {
		return nil, err
	}
	p := &filterParser[T]{toks: toks, fields: fields, filter: filter}
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

// lexFilter splits filter into words, quoted strings, operators and
// parentheses.
func lexFilter(filter string) ([]filterTok, error) {
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
				return nil, errInvalid("Invalid filter %q: unterminated string.", filter)
			}
			toks = append(toks, filterTok{tokString, b.String()})
			i = j + 1
		case strings.ContainsRune("=!<>:", rune(c)):
			op := string(c)
			if i+1 < len(filter) && filter[i+1] == '=' && c != '=' && c != ':' {
				op += "="
			}
			if op == "!" {
				return nil, errInvalid("Invalid filter %q: unexpected \"!\".", filter)
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

type filterParser[T any] struct {
	toks   []filterTok
	pos    int
	fields map[string]filterField[T]
	filter string
}

func (p *filterParser[T]) errorf(format string, args ...any) error {
	return errInvalid("Invalid filter %q: "+format+".", append([]any{p.filter}, args...)...)
}

func (p *filterParser[T]) peekWord(w string) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == tokWord && p.toks[p.pos].text == w
}

// or := and { OR and }
func (p *filterParser[T]) or() (func(T) bool, error) {
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
func (p *filterParser[T]) and() (func(T) bool, error) {
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
func (p *filterParser[T]) unary() (func(T) bool, error) {
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
func (p *filterParser[T]) term() (func(T) bool, error) {
	if p.pos+3 > len(p.toks) {
		return nil, p.errorf("expected a field, an operator and a value")
	}
	name, op, val := p.toks[p.pos], p.toks[p.pos+1], p.toks[p.pos+2]
	if name.kind != tokWord || op.kind != tokOp || (val.kind != tokWord && val.kind != tokString) {
		return nil, p.errorf("expected a field, an operator and a value at %q", name.text)
	}
	p.pos += 3
	f, ok := p.fields[name.text]
	if !ok {
		return nil, p.errorf("unknown field %q", name.text)
	}
	if !f.time {
		switch op.text {
		case "=":
			return func(v T) bool { return f.get(v) == val.text }, nil
		case "!=":
			return func(v T) bool { return f.get(v) != val.text }, nil
		case ":":
			return func(v T) bool { return strings.Contains(f.get(v), val.text) }, nil
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
		got, err := time.Parse(time.RFC3339Nano, f.get(v))
		if err != nil {
			return op.text == "!="
		}
		return cmp(got.Compare(want))
	}, nil
}
