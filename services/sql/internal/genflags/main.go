// Command genflags generates Cloud SQL for PostgreSQL's database flag
// allow-list (FR-SQL-003) from Google's documentation:
//
//	go run ./services/sql/internal/genflags [-in flags.html] -out services/sql/flags_docs.go
//
// It reads the flag table of https://cloud.google.com/sql/docs/postgres/flags
// (or a saved copy) and writes each flag's type, range or values, the first
// major version that supports it and whether changing it restarts the
// instance. The documentation is prose; flags whose semantics it can't
// express are overridden by hand in flags.go.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const docURL = "https://cloud.google.com/sql/docs/postgres/flags?hl=en"

func main() {
	in := flag.String("in", "", "saved documentation page (default: fetch "+docURL+")")
	out := flag.String("out", "flags_docs.go", "output Go file")
	flag.Parse()
	page, err := read(*in)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := flagTable(page)
	if err != nil {
		log.Fatal(err)
	}
	var defs []def
	for _, r := range rows {
		if d, ok := parseRow(r); ok {
			defs = append(defs, d)
		}
	}
	if len(defs) < 200 {
		log.Fatalf("only %d flags parsed; has the page layout changed?", len(defs))
	}
	src, err := render(defs)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d flags to %s", len(defs), *out)
}

func read(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	cl := &http.Client{Timeout: time.Minute}
	resp, err := cl.Get(docURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", docURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// flagTable returns the cells of the table whose header starts with
// "Cloud SQL Flag".
func flagTable(page []byte) ([][]string, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, err
	}
	var tables [][][]string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "table" {
			var rows [][]string
			var rowsOf func(n *html.Node)
			rowsOf = func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "table" && len(rows) > 0 {
					return // nested table
				}
				if n.Type == html.ElementNode && n.Data == "tr" {
					var cells []string
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
							cells = append(cells, text(c))
						}
					}
					rows = append(rows, cells)
					return
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					rowsOf(c)
				}
			}
			rowsOf(n)
			tables = append(tables, rows)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	for _, t := range tables {
		if len(t) > 1 && len(t[0]) >= 3 && strings.HasPrefix(t[0][0], "Cloud SQL Flag") {
			return t[1:], nil
		}
	}
	return nil, fmt.Errorf("flag table not found")
}

// text returns n's text with <br> and block ends as newlines.
func text(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.Type == html.ElementNode && (n.Data == "br" || n.Data == "p" || n.Data == "li" || n.Data == "div"):
			b.WriteString("\n")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

type def struct {
	Name    string
	Type    string // BOOLEAN, INTEGER, FLOAT, STRING
	Min     float64
	Max     float64
	Values  []string
	Restart bool
	Since   int
}

var (
	rangeRE  = regexp.MustCompile(`(-?[\d,]+(?:\.\d+)?)\s*\.\.\.\s*(-?[\d,]+(?:\.\d+)?|varies|inf)`)
	sinceRE  = regexp.MustCompile(`Supported in PostgreSQL (\d+) and later`)
	onlyRE   = regexp.MustCompile(`Supported only in PostgreSQL ([\d.]+)`)
	stopRE   = regexp.MustCompile(`(?i)\b(The default|Note|Supported|Set to|Additionally|You can|Can be set|Postgres \d)`)
	spacesRE = regexp.MustCompile(`\s+`)
)

func parseRow(r []string) (def, bool) {
	if len(r) < 3 {
		return def{}, false
	}
	name := strings.TrimSpace(strings.SplitN(strings.TrimSpace(r[0]), "\n", 2)[0])
	if name == "" || strings.ContainsAny(name, " \t") {
		return def{}, false
	}
	t := strings.TrimSpace(spacesRE.ReplaceAllString(r[1], " "))
	if onlyRE.MatchString(t) {
		return def{}, false // a release before every supported version
	}
	d := def{Name: name, Restart: strings.HasPrefix(strings.TrimSpace(r[2]), "Yes")}
	if m := sinceRE.FindStringSubmatch(t); m != nil {
		d.Since, _ = strconv.Atoi(m[1])
	}
	kind, rest, _ := strings.Cut(t, " ")
	switch strings.ToLower(strings.TrimRight(kind, ".")) {
	case "boolean":
		d.Type = "BOOLEAN"
	case "integer":
		d.Type, d.Min, d.Max = "INTEGER", math.MinInt32, math.MaxInt32
		if m := rangeRE.FindStringSubmatch(rest); m != nil {
			d.Min, d.Max = num(m[1], math.MinInt32), num(m[2], math.MaxInt32)
		}
		if strings.Contains(rest, "or -1") && d.Min > -1 {
			d.Min = -1
		}
	case "float":
		d.Type, d.Min, d.Max = "FLOAT", -math.MaxFloat64, math.MaxFloat64
		if m := rangeRE.FindStringSubmatch(rest); m != nil {
			d.Min, d.Max = num(m[1], -math.MaxFloat64), num(m[2], math.MaxFloat64)
		}
	case "enumeration", "string":
		d.Type = "STRING"
		d.Values = values(rest)
	default:
		return def{}, false
	}
	return d, true
}

func num(s string, dflt float64) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil || math.IsInf(f, 0) {
		return dflt
	}
	return f
}

// values extracts "a | b | 'c d'" style value lists; free-form strings
// return nil.
func values(s string) []string {
	if loc := stopRE.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	if !strings.Contains(s, "|") {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range strings.Split(s, "|") {
		v = strings.Trim(strings.TrimSpace(v), "'")
		if v == "" || seen[v] {
			continue
		}
		if len(v) > 32 || strings.ContainsAny(v, ".:") {
			return nil // prose, not a value list
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func render(defs []def) ([]byte, error) {
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	var b bytes.Buffer
	b.WriteString("// Code generated by services/sql/internal/genflags from " + docURL + "; DO NOT EDIT.\n\n")
	b.WriteString("package sql\n\n")
	b.WriteString("// docFlags is Cloud SQL for PostgreSQL's documented flag allow-list.\n")
	b.WriteString("var docFlags = map[string]flagDef{\n")
	f := func(v float64) string {
		switch {
		case v == math.MaxFloat64:
			return "math.MaxFloat64"
		case v == -math.MaxFloat64:
			return "-math.MaxFloat64"
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	for _, d := range defs {
		fmt.Fprintf(&b, "\t%q: {Type: %q", d.Name, d.Type)
		if d.Type == "INTEGER" || d.Type == "FLOAT" {
			fmt.Fprintf(&b, ", Min: %s, Max: %s", f(d.Min), f(d.Max))
		}
		if len(d.Values) > 0 {
			fmt.Fprintf(&b, ", Values: %#v", d.Values)
		}
		if d.Restart {
			b.WriteString(", Restart: true")
		}
		if d.Since > 0 {
			fmt.Fprintf(&b, ", Since: %d", d.Since)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n")
	src := b.String()
	if strings.Contains(src, "math.") {
		src = strings.Replace(src, "package sql\n\n", "package sql\n\nimport \"math\"\n\n", 1)
	}
	return format.Source([]byte(src))
}
