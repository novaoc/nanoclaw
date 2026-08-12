package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Compact inspection of Rails db/schema.rb so the model never has to read the
// whole generated file (often 30–50 KB and over the single-file read budget).

type schemaRB struct {
	Version string
	Order   []string
	Tables  map[string]*schemaTable
}

type schemaTable struct {
	Name    string
	Columns []schemaCol
	Indexes []schemaIdx
	Checks  []schemaCheck
	FKs     []schemaFK
	NoID    bool // create_table … id: false
}

type schemaCol struct {
	Name    string
	Type    string
	NotNull bool
	Default string // raw token, may be empty
	Limit   string
	Extra   string // other options compacted, e.g. precision/scale
}

type schemaIdx struct {
	Columns []string
	Name    string
	Unique  bool
	Where   string
}

type schemaCheck struct {
	Expr string
	Name string
}

type schemaFK struct {
	ToTable  string
	Column   string
	OnDelete string
}

var (
	schemaDefineRE     = regexp.MustCompile(`ActiveRecord::Schema(?:\[[^\]]+\])?\.define\s*\(\s*version:\s*([0-9_]+)`)
	createTableStartRE = regexp.MustCompile(`^\s*create_table\s+"([^"]+)"(.*)\s+do\s+\|t\|\s*$`)
	colLineRE          = regexp.MustCompile(`^\s*t\.([a-z_]+)\s+"([^"]+)"(.*)$`)
	indexLineRE        = regexp.MustCompile(`^\s*t\.index\s+(\[[^\]]+\]|"[^"]+")(.*)$`)
	checkLineRE        = regexp.MustCompile(`^\s*t\.check_constraint\s+"((?:\\.|[^"\\])*)"(.*)$`)
	fkLineRE           = regexp.MustCompile(`^\s*add_foreign_key\s+"([^"]+)"\s*,\s*"([^"]+)"(.*)$`)
	topCheckRE         = regexp.MustCompile(`^\s*add_check_constraint\s+"([^"]+)"\s*,\s*"((?:\\.|[^"\\])*)"(.*)$`)
	rubyStrRE          = regexp.MustCompile(`"((?:\\.|[^"\\])*)"`)
	optDefaultRE       = regexp.MustCompile(`\bdefault:\s*([^,]+)`)
	optLimitRE         = regexp.MustCompile(`\blimit:\s*([0-9]+)`)
	optNullRE          = regexp.MustCompile(`\bnull:\s*(true|false)`)
	optNameRE          = regexp.MustCompile(`\bname:\s*"([^"]+)"`)
	optUniqueRE        = regexp.MustCompile(`\bunique:\s*true\b`)
	optWhereRE         = regexp.MustCompile(`\bwhere:\s*"((?:\\.|[^"\\])*)"`)
	optColumnRE        = regexp.MustCompile(`\bcolumn:\s*"([^"]+)"`)
	optOnDeleteRE      = regexp.MustCompile(`\bon_delete:\s*:([a-z_]+)`)
	optIDFalseRE       = regexp.MustCompile(`\bid:\s*false\b`)
	arrayStrRE         = regexp.MustCompile(`"([^"]+)"`)
)

// parseSchemaRB parses a Rails schema dump. ok is false when the content is not
// a recognizable schema (missing define or no tables) — callers must not treat
// that as an empty database.
func parseSchemaRB(src string) (*schemaRB, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("db/schema.rb is empty")
	}
	s := &schemaRB{
		Tables: map[string]*schemaTable{},
	}
	if m := schemaDefineRE.FindStringSubmatch(src); m != nil {
		s.Version = m[1]
	}

	lines := strings.Split(src, "\n")
	var cur *schemaTable
	for _, line := range lines {
		if cur == nil {
			if m := createTableStartRE.FindStringSubmatch(line); m != nil {
				cur = &schemaTable{Name: m[1], NoID: optIDFalseRE.MatchString(m[2])}
				continue
			}
			if m := fkLineRE.FindStringSubmatch(line); m != nil {
				from, to, rest := m[1], m[2], m[3]
				t := s.ensure(from)
				col := ""
				if cm := optColumnRE.FindStringSubmatch(rest); cm != nil {
					col = cm[1]
				} else {
					// Rails default: singularized to_table + "_id" — keep opaque.
					col = singularizeRough(to) + "_id"
				}
				onDel := ""
				if dm := optOnDeleteRE.FindStringSubmatch(rest); dm != nil {
					onDel = dm[1]
				}
				t.FKs = append(t.FKs, schemaFK{ToTable: to, Column: col, OnDelete: onDel})
				continue
			}
			if m := topCheckRE.FindStringSubmatch(line); m != nil {
				t := s.ensure(m[1])
				expr := unescapeRubyStr(m[2])
				name := ""
				if nm := optNameRE.FindStringSubmatch(m[3]); nm != nil {
					name = nm[1]
				}
				t.Checks = append(t.Checks, schemaCheck{Expr: expr, Name: name})
				continue
			}
			continue
		}

		trim := strings.TrimSpace(line)
		if trim == "end" {
			if !cur.NoID {
				// Implicit primary key unless id: false.
				hasID := false
				for _, c := range cur.Columns {
					if c.Name == "id" {
						hasID = true
						break
					}
				}
				if !hasID {
					cur.Columns = append([]schemaCol{{Name: "id", Type: "bigint", NotNull: true, Extra: "pk"}}, cur.Columns...)
				}
			}
			s.Order = append(s.Order, cur.Name)
			s.Tables[cur.Name] = cur
			cur = nil
			continue
		}

		if m := colLineRE.FindStringSubmatch(line); m != nil {
			typ, name, rest := m[1], m[2], m[3]
			switch typ {
			case "index":
				// handled below — colLineRE can match t.index "col" single-arg form
			case "check_constraint":
				// handled below
			default:
				if typ == "timestamps" {
					// rare in dumps; expand
					cur.Columns = append(cur.Columns,
						schemaCol{Name: "created_at", Type: "datetime", NotNull: true},
						schemaCol{Name: "updated_at", Type: "datetime", NotNull: true},
					)
					continue
				}
				if typ == "references" || typ == "belongs_to" {
					c := schemaCol{Name: name + "_id", Type: "bigint"}
					applyColOpts(&c, rest)
					cur.Columns = append(cur.Columns, c)
					continue
				}
				c := schemaCol{Name: name, Type: typ}
				applyColOpts(&c, rest)
				cur.Columns = append(cur.Columns, c)
				continue
			}
		}

		if m := indexLineRE.FindStringSubmatch(line); m != nil {
			colsRaw, rest := m[1], m[2]
			idx := schemaIdx{}
			if strings.HasPrefix(colsRaw, "[") {
				for _, sm := range arrayStrRE.FindAllStringSubmatch(colsRaw, -1) {
					idx.Columns = append(idx.Columns, sm[1])
				}
			} else {
				idx.Columns = []string{strings.Trim(colsRaw, `"`)}
			}
			if nm := optNameRE.FindStringSubmatch(rest); nm != nil {
				idx.Name = nm[1]
			}
			idx.Unique = optUniqueRE.MatchString(rest)
			if wm := optWhereRE.FindStringSubmatch(rest); wm != nil {
				idx.Where = unescapeRubyStr(wm[1])
			}
			cur.Indexes = append(cur.Indexes, idx)
			continue
		}

		if m := checkLineRE.FindStringSubmatch(line); m != nil {
			ch := schemaCheck{Expr: unescapeRubyStr(m[1])}
			if nm := optNameRE.FindStringSubmatch(m[2]); nm != nil {
				ch.Name = nm[1]
			}
			cur.Checks = append(cur.Checks, ch)
			continue
		}
	}

	if len(s.Tables) == 0 {
		if s.Version == "" && !strings.Contains(src, "create_table") {
			return nil, fmt.Errorf("db/schema.rb is unparseable (not a Rails schema dump)")
		}
		return nil, fmt.Errorf("db/schema.rb is unparseable (no create_table blocks found)")
	}
	return s, nil
}

func (s *schemaRB) ensure(name string) *schemaTable {
	if t, ok := s.Tables[name]; ok {
		return t
	}
	t := &schemaTable{Name: name}
	s.Tables[name] = t
	// FKs may reference tables not yet seen; keep Order for create_table only.
	return t
}

func applyColOpts(c *schemaCol, rest string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return
	}
	if m := optNullRE.FindStringSubmatch(rest); m != nil {
		c.NotNull = m[1] == "false"
	}
	if m := optDefaultRE.FindStringSubmatch(rest); m != nil {
		c.Default = strings.TrimSpace(m[1])
	}
	if m := optLimitRE.FindStringSubmatch(rest); m != nil {
		c.Limit = m[1]
	}
	// Capture remaining simple keywords without bloating output.
	var extras []string
	if strings.Contains(rest, "precision:") {
		if m := regexp.MustCompile(`\bprecision:\s*([0-9]+)`).FindStringSubmatch(rest); m != nil {
			extras = append(extras, "precision="+m[1])
		}
	}
	if strings.Contains(rest, "scale:") {
		if m := regexp.MustCompile(`\bscale:\s*([0-9]+)`).FindStringSubmatch(rest); m != nil {
			extras = append(extras, "scale="+m[1])
		}
	}
	if strings.Contains(rest, "array: true") {
		extras = append(extras, "array")
	}
	if len(extras) > 0 {
		c.Extra = strings.Join(extras, ",")
	}
}

func unescapeRubyStr(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

// singularizeRough is only used when add_foreign_key omits column: — Rails
// default. Not a full inflection; good enough for suggestions in dump output.
func singularizeRough(table string) string {
	if strings.HasSuffix(table, "ies") && len(table) > 3 {
		return table[:len(table)-3] + "y"
	}
	if strings.HasSuffix(table, "ses") || strings.HasSuffix(table, "xes") || strings.HasSuffix(table, "zes") {
		return table[:len(table)-2]
	}
	if strings.HasSuffix(table, "s") && len(table) > 1 {
		return table[:len(table)-1]
	}
	return table
}

// formatSchemaInspect renders a compact, stable describe/list result.
// tables empty → list names only. Unknown names are reported with near matches.
func formatSchemaInspect(s *schemaRB, tables []string) string {
	var b strings.Builder
	ver := s.Version
	if ver == "" {
		ver = "?"
	}
	fmt.Fprintf(&b, "db/schema.rb version=%s\n", ver)

	if len(tables) == 0 {
		names := append([]string(nil), s.Order...)
		// Include any FK-only stubs not in Order (shouldn't happen for real dumps).
		for name := range s.Tables {
			if _, ok := findStr(names, name); !ok {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		// Prefer create_table order when it covers everything.
		if len(s.Order) == len(s.Tables) {
			names = append([]string(nil), s.Order...)
		}
		fmt.Fprintf(&b, "tables (%d): %s\n", len(names), strings.Join(names, ", "))
		return strings.TrimRight(b.String(), "\n")
	}

	allNames := s.Order
	if len(allNames) == 0 {
		for n := range s.Tables {
			allNames = append(allNames, n)
		}
		sort.Strings(allNames)
	}

	for i, want := range tables {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		t, ok := s.Tables[want]
		if !ok {
			// case-insensitive hit
			for _, n := range allNames {
				if strings.EqualFold(n, want) {
					t, ok = s.Tables[n], true
					want = n
					break
				}
			}
		}
		if !ok {
			near := closestTableNames(want, allNames, 5)
			fmt.Fprintf(&b, "unknown table %q", want)
			if len(near) > 0 {
				fmt.Fprintf(&b, " — closest: %s", strings.Join(near, ", "))
			}
			b.WriteByte('\n')
			continue
		}
		writeTableDescribe(&b, t)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeTableDescribe(b *strings.Builder, t *schemaTable) {
	fmt.Fprintf(b, "%s\n", t.Name)
	// columns — one line
	b.WriteString("  cols: ")
	if len(t.Columns) == 0 {
		b.WriteString("(none)")
	} else {
		for i, c := range t.Columns {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(formatCol(c))
		}
	}
	b.WriteByte('\n')

	b.WriteString("  idx: ")
	if len(t.Indexes) == 0 {
		b.WriteString("(none)")
	} else {
		for i, idx := range t.Indexes {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(formatIdx(idx))
		}
	}
	b.WriteByte('\n')

	b.WriteString("  fk: ")
	if len(t.FKs) == 0 {
		b.WriteString("(none)")
	} else {
		for i, fk := range t.FKs {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(formatFK(fk))
		}
	}
	b.WriteByte('\n')

	b.WriteString("  check: ")
	if len(t.Checks) == 0 {
		b.WriteString("(none)")
	} else {
		for i, ch := range t.Checks {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(formatCheck(ch))
		}
	}
	b.WriteByte('\n')
}

func formatCol(c schemaCol) string {
	var b strings.Builder
	b.WriteString(c.Name)
	b.WriteByte(':')
	b.WriteString(c.Type)
	if c.Limit != "" {
		fmt.Fprintf(&b, "(%s)", c.Limit)
	}
	if c.NotNull {
		b.WriteString(" not_null")
	}
	if c.Default != "" {
		b.WriteString(" default=")
		b.WriteString(strings.TrimSpace(c.Default))
	}
	if c.Extra != "" {
		b.WriteByte(' ')
		b.WriteString(c.Extra)
	}
	return b.String()
}

func formatIdx(idx schemaIdx) string {
	var b strings.Builder
	if idx.Unique {
		b.WriteString("unique")
	} else {
		b.WriteString("index")
	}
	b.WriteByte('(')
	b.WriteString(strings.Join(idx.Columns, ","))
	b.WriteByte(')')
	if idx.Where != "" {
		b.WriteString(" where=")
		// keep short
		w := idx.Where
		if len(w) > 60 {
			w = w[:57] + "..."
		}
		b.WriteString(w)
	}
	return b.String()
}

func formatFK(fk schemaFK) string {
	s := fk.Column + "->" + fk.ToTable
	if fk.OnDelete != "" {
		s += " on_delete=" + fk.OnDelete
	}
	return s
}

func formatCheck(ch schemaCheck) string {
	expr := ch.Expr
	if len(expr) > 80 {
		expr = expr[:77] + "..."
	}
	if ch.Name != "" {
		return ch.Name + ": " + expr
	}
	return expr
}

func findStr(ss []string, want string) (int, bool) {
	for i, s := range ss {
		if s == want {
			return i, true
		}
	}
	return -1, false
}

// closestTableNames ranks existing names by edit distance / containment.
func closestTableNames(want string, names []string, n int) []string {
	if len(names) == 0 || n <= 0 {
		return nil
	}
	want = strings.ToLower(strings.TrimSpace(want))
	type scored struct {
		name  string
		score int
	}
	var ranked []scored
	for _, name := range names {
		ln := strings.ToLower(name)
		d := levenshtein(want, ln)
		// Prefer containment / shared prefix.
		if strings.Contains(ln, want) || strings.Contains(want, ln) {
			if d > 2 {
				d = 2
			}
		}
		// Cap absurd distances so we still surface something for typos.
		ranked = append(ranked, scored{name: name, score: d})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		return ranked[i].name < ranked[j].name
	})
	// Drop very far matches unless nothing is close.
	maxDist := 6
	if len(want) <= 4 {
		maxDist = 3
	}
	var out []string
	for _, r := range ranked {
		if r.score > maxDist && len(out) >= 1 {
			break
		}
		out = append(out, r.name)
		if len(out) >= n {
			break
		}
	}
	return out
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return len(b)
	}
	if b == "" {
		return len(a)
	}
	// Ensure b is shorter row for memory — actually use two rows on len(b).
	la, lb := len(a), len(b)
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		ca := a[i-1]
		for j := 1; j <= lb; j++ {
			cost := 1
			if ca == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

// normalizeTableArgs accepts comma-separated blobs and trims empties.
func normalizeTableArgs(tables []string) []string {
	var out []string
	for _, t := range tables {
		for _, p := range strings.Split(t, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
