package iam

import (
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed roles.yaml
var rolesYAML []byte

// predefinedRole is one entry of the embedded catalogue.
type predefinedRole struct {
	Name        string
	Title       string
	Description string
	Stage       string
	// Permissions is the expanded, sorted permission list.
	Permissions []string
	basic       string
	include     []func(string) bool
	exclude     []func(string) bool
}

// has reports whether the role grants perm. Patterns are evaluated at check
// time so that a permission a service checks but the catalogue does not
// list is still covered by wildcard and basic roles.
func (r *predefinedRole) has(perm string) bool {
	ok := r.basic != "" && basicIncludes(r.basic, perm)
	for _, m := range r.include {
		if ok {
			break
		}
		ok = m(perm)
	}
	if !ok {
		return false
	}
	for _, m := range r.exclude {
		if m(perm) {
			return false
		}
	}
	return true
}

// catalogue is the embedded role catalogue (FR-IAM-002).
type catalogue struct {
	universe []string // every known permission, sorted
	known    map[string]bool
	services map[string]bool // permission service prefixes in the catalogue
	roles    map[string]*predefinedRole
	names    []string // role names, sorted
}

type catalogueFile struct {
	Permissions map[string][]string `yaml:"permissions"`
	Roles       []struct {
		Name        string   `yaml:"name"`
		Title       string   `yaml:"title"`
		Description string   `yaml:"description"`
		Stage       string   `yaml:"stage"`
		Basic       string   `yaml:"basic"`
		Permissions []string `yaml:"permissions"`
	} `yaml:"roles"`
}

// loadCatalogue parses and expands the embedded catalogue.
func loadCatalogue(data []byte) (*catalogue, error) {
	var f catalogueFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	c := &catalogue{known: map[string]bool{}, roles: map[string]*predefinedRole{}, services: map[string]bool{}}
	for svc, groups := range f.Permissions {
		c.services[svc] = true
		for _, g := range groups {
			typ, verbs, ok := strings.Cut(g, ":")
			if !ok {
				return nil, fmt.Errorf("roles.yaml: bad permission group %q", g)
			}
			for _, v := range strings.Fields(verbs) {
				c.known[svc+"."+strings.TrimSpace(typ)+"."+v] = true
			}
		}
	}
	// Exact permissions named by roles join the universe too.
	for _, r := range f.Roles {
		for _, p := range r.Permissions {
			p = strings.TrimPrefix(p, "-")
			if !strings.Contains(p, "*") {
				c.known[p] = true
			}
		}
	}
	for p := range c.known {
		c.universe = append(c.universe, p)
	}
	sort.Strings(c.universe)

	for _, r := range f.Roles {
		pr := &predefinedRole{Name: r.Name, Title: r.Title, Description: r.Description, Stage: r.Stage, basic: r.Basic}
		if pr.Stage == "" {
			pr.Stage = "GA"
		}
		for _, pat := range r.Permissions {
			if neg, ok := strings.CutPrefix(pat, "-"); ok {
				pr.exclude = append(pr.exclude, compilePattern(neg))
				continue
			}
			match := compilePattern(pat)
			if !slices.ContainsFunc(c.universe, match) {
				return nil, fmt.Errorf("roles.yaml: %s: pattern %q matches nothing", r.Name, pat)
			}
			pr.include = append(pr.include, match)
		}
		for _, p := range c.universe {
			if pr.has(p) {
				pr.Permissions = append(pr.Permissions, p)
			}
		}
		c.roles[pr.Name] = pr
		c.names = append(c.names, pr.Name)
	}
	sort.Strings(c.names)
	return c, nil
}

// compilePattern returns a matcher for an exact permission or a pattern
// where a leading or trailing "*" spans any number of segments and an inner
// "*" exactly one.
func compilePattern(pattern string) func(string) bool {
	if !strings.Contains(pattern, "*") {
		return func(p string) bool { return p == pattern }
	}
	segs := strings.Split(pattern, ".")
	for i, s := range segs {
		switch {
		case s == "*" && (i == 0 || i == len(segs)-1):
			segs[i] = ".+"
		case s == "*":
			segs[i] = "[^.]+"
		default:
			segs[i] = regexp.QuoteMeta(s)
		}
	}
	return regexp.MustCompile("^" + strings.Join(segs, `\.`) + "$").MatchString
}

// Verbs basic viewers get, and permissions editors lack. Basic roles are
// computed by rule over the universe rather than listed (FR-IAM-002).
var (
	viewerVerb       = regexp.MustCompile(`^(get|list)`)
	viewerExcluded   = map[string]bool{"getAccessToken": true, "getOpenIdToken": true}
	editorExcludedRe = regexp.MustCompile(`\.(setIamPolicy)$|^iam\.roles\.(create|delete|undelete|update)$|^iam\.serviceAccounts\.(getAccessToken|getOpenIdToken|implicitDelegation|signBlob|signJwt|setIamPolicy)$|^resourcemanager\.projects\.(delete|undelete|move|create)$|^container\.(clusterRoles|roles)\.(bind|escalate)$|^storage\.objects\.(overrideUnlockedRetention)$|^secretmanager\.versions\.access$`)
)

// basicIncludes reports whether a basic role includes perm.
func basicIncludes(basic, perm string) bool {
	switch basic {
	case "owner":
		return true
	case "editor":
		return !editorExcludedRe.MatchString(perm)
	case "viewer":
		verb := perm[strings.LastIndexByte(perm, '.')+1:]
		return viewerVerb.MatchString(verb) && !viewerExcluded[verb]
	}
	return false
}

// builtin returns the embedded catalogue, parsed once.
var builtin = sync.OnceValue(func() *catalogue {
	c, err := loadCatalogue(rolesYAML)
	if err != nil {
		panic(err) // the embedded file is covered by tests
	}
	return c
})
