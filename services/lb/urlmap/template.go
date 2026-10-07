package urlmap

import (
	"fmt"
	"regexp"
	"strings"
)

// Path templates (pathTemplateMatch / pathTemplateRewrite): "/" separated
// segments of literals, "*" (exactly one segment), "**" (zero or more
// segments, last only) and variables "{name}", "{name=*}", "{name=**}" or
// "{name=pattern}" capturing the segments they match.

type segKind int

const (
	segLit segKind = iota
	segStar
	segDStar
)

type tseg struct {
	kind segKind
	lit  string
	v    string // variable this segment belongs to ("" for none)
}

type template struct {
	segs []tseg
	vars []string
}

var varNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// parseTemplate parses a pathTemplateMatch pattern.
func parseTemplate(p string) (*template, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("path template %q must start with /", p)
	}
	if len(p) > 255 {
		return nil, fmt.Errorf("path template %q is longer than 255 characters", p)
	}
	t := &template{}
	rest := p[1:]
	if rest == "" {
		return t, nil
	}
	ops := 0
	for len(rest) > 0 {
		var part string
		if strings.HasPrefix(rest, "{") {
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				return nil, fmt.Errorf("path template %q has an unterminated variable", p)
			}
			part, rest = rest[:end+1], rest[end+1:]
			if rest != "" && !strings.HasPrefix(rest, "/") {
				return nil, fmt.Errorf("path template %q: a variable must span whole segments", p)
			}
			rest = strings.TrimPrefix(rest, "/")
			inner := part[1 : len(part)-1]
			name, pat, hasPat := strings.Cut(inner, "=")
			if !hasPat {
				pat = "*"
			}
			if !varNameRE.MatchString(name) {
				return nil, fmt.Errorf("path template %q: invalid variable name %q", p, name)
			}
			for _, v := range t.vars {
				if v == name {
					return nil, fmt.Errorf("path template %q: duplicate variable %q", p, name)
				}
			}
			t.vars = append(t.vars, name)
			for _, sp := range strings.Split(pat, "/") {
				sg, err := parseSeg(p, sp)
				if err != nil {
					return nil, err
				}
				if sg.kind != segLit {
					ops++
				}
				sg.v = name
				t.segs = append(t.segs, sg)
			}
			continue
		}
		seg, after, _ := strings.Cut(rest, "/")
		rest = after
		sg, err := parseSeg(p, seg)
		if err != nil {
			return nil, err
		}
		if sg.kind != segLit {
			ops++
		}
		t.segs = append(t.segs, sg)
	}
	for i, sg := range t.segs {
		if sg.kind == segDStar && i != len(t.segs)-1 {
			return nil, fmt.Errorf("path template %q: ** must be the last operator", p)
		}
	}
	if ops > 5 {
		return nil, fmt.Errorf("path template %q has more than 5 operators", p)
	}
	return t, nil
}

func parseSeg(p, s string) (tseg, error) {
	switch {
	case s == "*":
		return tseg{kind: segStar}, nil
	case s == "**":
		return tseg{kind: segDStar}, nil
	case s == "" || strings.ContainsAny(s, "*{}"):
		return tseg{}, fmt.Errorf("path template %q has an invalid segment %q", p, s)
	}
	return tseg{kind: segLit, lit: s}, nil
}

// match matches path and returns the variable captures.
func (t *template) match(path string) (map[string]string, bool) {
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	var segs []string
	if path != "/" {
		segs = strings.Split(path[1:], "/")
	}
	caps := map[string][]string{}
	i := 0
	for _, sg := range t.segs {
		switch sg.kind {
		case segLit:
			if i >= len(segs) || segs[i] != sg.lit {
				return nil, false
			}
			caps[sg.v] = append(caps[sg.v], segs[i])
			i++
		case segStar:
			if i >= len(segs) || segs[i] == "" {
				return nil, false
			}
			caps[sg.v] = append(caps[sg.v], segs[i])
			i++
		case segDStar:
			caps[sg.v] = append(caps[sg.v], segs[i:]...)
			i = len(segs)
		}
	}
	if i != len(segs) {
		return nil, false
	}
	out := map[string]string{}
	for _, v := range t.vars {
		out[v] = strings.Join(caps[v], "/")
	}
	return out, true
}

// rewriteVars lists the variables a rewrite template references.
func rewriteVars(r string) ([]string, error) {
	if !strings.HasPrefix(r, "/") {
		return nil, fmt.Errorf("path template rewrite %q must start with /", r)
	}
	var out []string
	rest := r
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return nil, fmt.Errorf("path template rewrite %q has an unterminated variable", r)
		}
		name := rest[i+1 : i+j]
		if !varNameRE.MatchString(name) {
			return nil, fmt.Errorf("path template rewrite %q: invalid variable %q", r, name)
		}
		out = append(out, name)
		rest = rest[i+j+1:]
	}
	if strings.ContainsAny(rest, "*") {
		return nil, fmt.Errorf("path template rewrite %q may not contain wildcards", r)
	}
	return out, nil
}

// expandTemplate substitutes captured variables into a rewrite template.
func expandTemplate(r string, vars map[string]string) string {
	var b strings.Builder
	rest := r
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		b.WriteString(vars[rest[i+1:i+j]])
		rest = rest[i+j+1:]
	}
	out := b.String()
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	return out
}
