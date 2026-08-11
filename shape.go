package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Post-generation shaping: stamp identity, rewrite the app README, and omit
// foundation modules the app_spec did not select. Pure transforms live here;
// GitHub application is in shape_remote.go.

const (
	foundationYMLPath = "config/foundation.yml"
	readmePath        = "README.md"
	identityStart     = "<!-- foundation:identity -->"
	identityEnd       = "<!-- /foundation:identity -->"
	moduleManifestDir = "config/foundation/modules"
)

// AppIdentity is the product identity stamped into foundation.yml + README.
type AppIdentity struct {
	ApplicationName string
	Description     string
	Domain          string
	SupportEmail    string
}

// ModuleManifest is a full foundation module declaration (paths, markers, etc.).
type ModuleManifest struct {
	Name             string
	Summary          string
	Default          string
	Paths            []string
	TablePrefixes    []string
	ConfigKeys       []string
	ResiduePatterns  []string
	DependsOn        []string
	SoftReferences   []string
	SourcePath       string
}

// ShapePlan is the set of file writes/deletes to apply to a generated app.
type ShapePlan struct {
	Writes  map[string]string // path → new content
	Deletes []string          // paths to remove
	Omitted []string          // module names omitted
	Kept    []string          // module names kept
}

// identityFromSpec builds a deploy-safe identity from the app spec and repo name.
// Domain is the Holodex demo host when the sandbox URL is known; never example.com.
// When no app_spec (or empty name/purpose) is present, values are derived from the
// repository name so placeholders never survive create_rails_app.
func identityFromSpec(spec *AppSpec, repoName, sandboxURL string) AppIdentity {
	name := ""
	desc := ""
	if spec != nil {
		name = strings.TrimSpace(spec.Name)
		desc = strings.TrimSpace(spec.Purpose)
	}
	if name == "" {
		name = humanizeRepoName(repoName)
	}
	name = sanitizeIdentityName(name)
	if name == "Application" {
		// Last resort: sanitized humanized slug (still better than the template default).
		if alt := sanitizeIdentityName(humanizeRepoName(repoName)); alt != "" && alt != "Application" {
			name = alt
		}
	}
	if desc == "" {
		if name != "" && name != "Application" {
			desc = name + "."
		} else {
			desc = "A generated Rails application."
		}
	}
	desc = sanitizeIdentityDescription(desc)
	domain := demoDomain(repoName, sandboxURL)
	email := "support@" + domain
	return AppIdentity{
		ApplicationName: name,
		Description:     desc,
		Domain:          domain,
		SupportEmail:    email,
	}
}

// humanizeRepoName turns "driftline-coffee" into "Driftline Coffee".
func humanizeRepoName(repoName string) string {
	s := repoSlug(repoName)
	if s == "" || s == "app" {
		return "App"
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	out := strings.TrimSpace(strings.Join(parts, " "))
	if out == "" {
		return "App"
	}
	return out
}

func demoDomain(repoName, sandboxURL string) string {
	slug := repoSlug(repoName)
	host := "demo.holode.xyz"
	if u, err := url.Parse(strings.TrimSpace(sandboxURL)); err == nil && u.Host != "" {
		host = u.Host
	}
	return slug + "." + host
}

func repoSlug(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	s := chartSlug(name)
	if s == "" || s == "chart" {
		s = "app"
	}
	return s
}

// sanitizeIdentityName mirrors bin/rename --name allowlist (printable ASCII subset).
func sanitizeIdentityName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r > 127 || !unicode.IsPrint(r) {
			continue
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == ' ', r == '\'', r == '.', r == '-':
			b.WriteRune(r)
		}
	}
	out := collapseSpaces(b.String())
	if out == "" {
		return "Application"
	}
	if len(out) > 60 {
		out = strings.TrimSpace(out[:60])
	}
	// Must start with letter or digit.
	if c := out[0]; !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
		return "Application"
	}
	return out
}

func sanitizeIdentityDescription(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r > 127 || !unicode.IsPrint(r) {
			continue
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == ' ', r == '\'', r == ',', r == '.', r == ':', r == '-':
			b.WriteRune(r)
		}
	}
	out := collapseSpaces(b.String())
	if out == "" {
		return "A generated Rails application."
	}
	if len(out) > 200 {
		out = strings.TrimSpace(out[:200])
	}
	if c := out[0]; !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
		return "A generated Rails application."
	}
	return out
}

func collapseSpaces(s string) string {
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// stampFoundationYML sets identity scalar keys (2-space indent under shared:).
func stampFoundationYML(src string, id AppIdentity) (string, error) {
	pairs := []struct{ key, val string }{
		{"application_name", id.ApplicationName},
		{"default_page_description", id.Description},
		{"domain", id.Domain},
		{"support_email", id.SupportEmail},
		{"legal_email", "legal@" + id.Domain},
	}
	out := src
	for _, p := range pairs {
		next, err := replaceYAMLScalar(out, p.key, p.val)
		if err != nil {
			return "", err
		}
		out = next
	}
	return out, nil
}

func replaceYAMLScalar(src, key, value string) (string, error) {
	re := regexp.MustCompile(`(?m)^(  ` + regexp.QuoteMeta(key) + `:)[^\n]*$`)
	locs := re.FindAllStringIndex(src, -1)
	if len(locs) != 1 {
		return "", fmt.Errorf("%s: expected exactly one %q entry, found %d", foundationYMLPath, key+":", len(locs))
	}
	// Value is allowlisted — safe inside double quotes.
	return re.ReplaceAllString(src, `${1} "`+value+`"`), nil
}

// stampREADMEIdentity rewrites the foundation:identity block and preserves markers.
func stampREADMEIdentity(src string, id AppIdentity) (string, error) {
	if strings.Count(src, identityStart) != 1 || strings.Count(src, identityEnd) != 1 {
		return "", fmt.Errorf("%s: expected exactly one %s ... %s block", readmePath, identityStart, identityEnd)
	}
	block := identityBlock(id)
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(identityStart) + `.*?` + regexp.QuoteMeta(identityEnd))
	out := re.ReplaceAllString(src, block)
	if !strings.Contains(out, block) {
		return "", fmt.Errorf("%s: identity block did not take the new values", readmePath)
	}
	return out, nil
}

func identityBlock(id AppIdentity) string {
	return strings.Join([]string{
		identityStart,
		"# " + id.ApplicationName,
		"",
		id.Description,
		"",
		"- Site: https://" + id.Domain,
		"- Support: " + id.SupportEmail,
		identityEnd,
	}, "\n")
}

// buildAppREADME replaces the foundation-describing README with a short app README.
// Preserves the identity marker block at the top.
func buildAppREADME(spec *AppSpec, id AppIdentity) string {
	var b strings.Builder
	b.WriteString(identityBlock(id))
	b.WriteString("\n\n")

	purpose := id.Description
	if spec != nil && strings.TrimSpace(spec.Purpose) != "" {
		purpose = strings.TrimSpace(spec.Purpose)
	}
	b.WriteString("## What this is\n\n")
	b.WriteString(purpose)
	b.WriteString("\n\n")

	if spec != nil && len(spec.Actors) > 0 {
		b.WriteString("## Who it is for\n\n")
		for _, a := range spec.Actors {
			if s := strings.TrimSpace(a); s != "" {
				fmt.Fprintf(&b, "- %s\n", s)
			}
		}
		b.WriteString("\n")
	}

	if spec != nil && len(spec.Workflows) > 0 {
		b.WriteString("## Main features\n\n")
		for _, w := range spec.Workflows {
			name := strings.TrimSpace(w.Name)
			if name == "" {
				continue
			}
			if d := strings.TrimSpace(w.Description); d != "" {
				fmt.Fprintf(&b, "- **%s** — %s\n", name, d)
			} else {
				fmt.Fprintf(&b, "- **%s**\n", name)
			}
		}
		b.WriteString("\n")
	}

	if spec != nil && len(spec.Entities) > 0 {
		b.WriteString("## Core entities\n\n")
		for _, e := range spec.Entities {
			if s := strings.TrimSpace(e.Name); s != "" {
				fmt.Fprintf(&b, "- %s\n", s)
			}
		}
		b.WriteString("\n")
	}

	if spec != nil && len(spec.Modules) > 0 {
		b.WriteString("## Included foundation modules\n\n")
		for _, m := range spec.Modules {
			if s := strings.TrimSpace(m); s != "" {
				fmt.Fprintf(&b, "- %s\n", s)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Run locally\n\n")
	b.WriteString("```bash\nbundle install\nbin/rails db:prepare\nbin/dev\n```\n\n")
	b.WriteString("Requires Ruby, PostgreSQL, and the usual Rails toolchain. See `bin/setup` if present.\n\n")

	if spec != nil && strings.TrimSpace(spec.SeedDemo) != "" {
		b.WriteString("## Demo\n\n")
		b.WriteString(strings.TrimSpace(spec.SeedDemo))
		b.WriteString("\n\n")
	}

	b.WriteString("## Deploy notes\n\n")
	b.WriteString("Production `config.hosts` is derived from `domain` in `config/foundation.yml`. ")
	b.WriteString("Keep that value aligned with the real host or every request will 403.\n")
	return b.String()
}

// loadFullModuleManifests reads every module YAML under foundationRoot.
func loadFullModuleManifests(root string) (map[string]*ModuleManifest, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("foundation root is not configured (set VELA_FOUNDATION_ROOT)")
	}
	dir := filepath.Join(root, moduleManifestDir)
	entries, err := readDirNames(dir)
	if err != nil {
		return nil, fmt.Errorf("reading foundation modules at %s: %w", dir, err)
	}
	out := map[string]*ModuleManifest{}
	for _, name := range entries {
		if !strings.HasSuffix(name, ".yml") {
			continue
		}
		raw, err := readFileString(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		m, err := parseFullModuleManifest(raw, filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out[m.Name] = m
	}
	return out, nil
}

// parseFullModuleManifest is a minimal YAML subset parser for module manifests.
func parseFullModuleManifest(content, sourcePath string) (*ModuleManifest, error) {
	m := &ModuleManifest{SourcePath: sourcePath, Default: "included"}
	lines := strings.Split(content, "\n")
	var listKey string
	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		// List item under current key.
		if strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t") {
			trim := strings.TrimSpace(raw)
			if listKey == "" || !strings.HasPrefix(trim, "-") {
				continue
			}
			val := strings.TrimSpace(strings.TrimPrefix(trim, "-"))
			val = strings.Trim(val, `"'`)
			switch listKey {
			case "paths":
				m.Paths = append(m.Paths, val)
			case "table_prefixes":
				m.TablePrefixes = append(m.TablePrefixes, val)
			case "config_keys":
				m.ConfigKeys = append(m.ConfigKeys, val)
			case "residue_patterns":
				m.ResiduePatterns = append(m.ResiduePatterns, val)
			case "depends_on":
				m.DependsOn = append(m.DependsOn, val)
			case "soft_references":
				m.SoftReferences = append(m.SoftReferences, val)
			}
			continue
		}
		listKey = ""
		key, val, ok := strings.Cut(raw, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			m.Name = val
		case "summary":
			m.Summary = val
		case "default":
			if val != "" {
				m.Default = val
			}
		case "paths", "table_prefixes", "config_keys", "residue_patterns", "depends_on", "soft_references":
			listKey = key
			// Inline empty list: key: []
			if val == "[]" || val == "" {
				// stay in list mode only for following indented items; empty is fine
				if val == "[]" {
					listKey = ""
				}
			}
		}
	}
	if m.Name == "" {
		return nil, fmt.Errorf("module manifest %s has no name", sourcePath)
	}
	if len(m.Paths) == 0 {
		return nil, fmt.Errorf("module manifest %s: paths must be a non-empty list", sourcePath)
	}
	return m, nil
}

// planShape builds writes/deletes for identity + README + module omission.
// files is the current repo path→content map (only paths that exist).
// declared is every module the foundation ships; keep is app_spec.Modules.
// When spec is nil, all declared modules are kept (identity + README only).
func planShape(files map[string]string, spec *AppSpec, id AppIdentity, declared map[string]*ModuleManifest) (*ShapePlan, error) {
	if declared == nil {
		declared = map[string]*ModuleManifest{}
	}
	keepSet := map[string]bool{}
	// No app_spec → keep every module; selection is unknown, identity still stamps.
	keepAll := spec == nil
	if !keepAll {
		for _, n := range spec.Modules {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if _, ok := declared[n]; !ok {
				return nil, fmt.Errorf("unknown module %q — cannot shape; foundation declares: %s",
					n, orNoneList(sortedManifestNames(declared)))
			}
			keepSet[n] = true
		}
	}

	plan := &ShapePlan{Writes: map[string]string{}}
	for name := range declared {
		if keepAll || keepSet[name] {
			plan.Kept = append(plan.Kept, name)
		} else {
			plan.Omitted = append(plan.Omitted, name)
		}
	}
	sort.Strings(plan.Kept)
	sort.Strings(plan.Omitted)

	// Working copy of file contents for sequential omits.
	work := map[string]string{}
	for k, v := range files {
		work[k] = v
	}

	// Omit unselected modules (dependency order: dependents first).
	omitOrder, err := omitOrder(plan.Omitted, declared)
	if err != nil {
		return nil, err
	}
	for _, name := range omitOrder {
		m := declared[name]
		if err := omitModuleInMap(work, m, declared); err != nil {
			return nil, err
		}
	}

	if err := applyIdentityAndREADME(work, spec, id); err != nil {
		return nil, err
	}
	diffShapePlan(plan, files, work)
	return plan, nil
}

// planIdentityOnly stamps foundation.yml + app README without omitting modules.
// Used when omission cannot be done safely (or there is no module selection).
func planIdentityOnly(files map[string]string, spec *AppSpec, id AppIdentity, declared map[string]*ModuleManifest) (*ShapePlan, error) {
	if declared == nil {
		declared = map[string]*ModuleManifest{}
	}
	plan := &ShapePlan{Writes: map[string]string{}}
	for name := range declared {
		plan.Kept = append(plan.Kept, name)
	}
	sort.Strings(plan.Kept)

	work := map[string]string{}
	for k, v := range files {
		work[k] = v
	}
	if err := applyIdentityAndREADME(work, spec, id); err != nil {
		return nil, err
	}
	diffShapePlan(plan, files, work)
	return plan, nil
}

func applyIdentityAndREADME(work map[string]string, spec *AppSpec, id AppIdentity) error {
	yml, ok := work[foundationYMLPath]
	if !ok {
		return fmt.Errorf("%s missing from generated app — cannot stamp identity", foundationYMLPath)
	}
	yml2, err := stampFoundationYML(yml, id)
	if err != nil {
		return err
	}
	work[foundationYMLPath] = yml2
	work[readmePath] = buildAppREADME(spec, id)
	return nil
}

func diffShapePlan(plan *ShapePlan, orig, work map[string]string) {
	if plan.Writes == nil {
		plan.Writes = map[string]string{}
	}
	for path, content := range work {
		prev, existed := orig[path]
		if !existed || prev != content {
			plan.Writes[path] = content
		}
	}
	for path := range orig {
		if _, ok := work[path]; !ok {
			plan.Deletes = append(plan.Deletes, path)
		}
	}
	sort.Strings(plan.Deletes)
}

func sortedManifestNames(m map[string]*ModuleManifest) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// omitOrder puts modules that others depend on later (omit dependents first).
func omitOrder(names []string, declared map[string]*ModuleManifest) ([]string, error) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	// Refuse omitting a dependency while a kept module still needs it —
	// handled in omitModuleInMap via assert_dependencies. Here just stable sort
	// with dependents before dependencies among the omit set.
	var out []string
	remaining := map[string]bool{}
	for n := range set {
		remaining[n] = true
	}
	for len(remaining) > 0 {
		progress := false
		var batch []string
		for n := range remaining {
			// Can omit n now if no other remaining module depends on n.
			blocked := false
			for other := range remaining {
				if other == n {
					continue
				}
				for _, dep := range declared[other].DependsOn {
					if dep == n {
						blocked = true
						break
					}
				}
				if blocked {
					break
				}
			}
			if !blocked {
				batch = append(batch, n)
			}
		}
		if len(batch) == 0 {
			// cycle among omit set — any order; dependency check will catch kept deps
			for n := range remaining {
				batch = append(batch, n)
			}
		}
		sort.Strings(batch)
		for _, n := range batch {
			out = append(out, n)
			delete(remaining, n)
			progress = true
		}
		if !progress {
			break
		}
	}
	return out, nil
}

// omitModuleInMap applies foundation Omit semantics to an in-memory tree.
func omitModuleInMap(files map[string]string, m *ModuleManifest, declared map[string]*ModuleManifest) error {
	// Dependents still present (manifest file still in tree) block omission.
	for name, other := range declared {
		if name == m.Name {
			continue
		}
		manifestPath := moduleManifestDir + "/" + name + ".yml"
		if _, stillThere := files[manifestPath]; !stillThere {
			continue // already omitted
		}
		for _, dep := range other.DependsOn {
			if dep == m.Name {
				return fmt.Errorf("cannot omit %s: still required by %s", m.Name, name)
			}
		}
	}

	// 1. Delete owned paths (file or directory prefix).
	var toDelete []string
	for path := range files {
		if ownedPath(m, path) {
			toDelete = append(toDelete, path)
		}
	}
	for _, p := range toDelete {
		delete(files, p)
	}

	// 2. Delete the manifest itself.
	delete(files, moduleManifestDir+"/"+m.Name+".yml")

	// 3–6. Rewrite surviving text files under scan roots.
	allow := omitAllowlisted
	scanRoots := []string{"app/", "bin/", "config/", "db/", "docs/", "lib/", "script/", "test/", "README.md"}
	for path, content := range files {
		if allow(path) {
			continue
		}
		under := false
		for _, root := range scanRoots {
			if path == strings.TrimSuffix(root, "/") || strings.HasPrefix(path, root) || path == "README.md" {
				under = true
				break
			}
		}
		if !under {
			continue
		}
		updated := stripModuleMarkers(content, m.Name)
		if strings.HasSuffix(path, ".css") {
			updated = stripCSSPrefixLines(updated, m.ResiduePatterns)
		}
		if path == foundationYMLPath {
			updated = stripFoundationYMLKeys(updated, m.ConfigKeys)
		}
		if path == "db/schema.rb" {
			updated = stripSchemaTables(updated, m.TablePrefixes)
		}
		if updated != content {
			files[path] = updated
		}
	}

	// 7. Residue scan.
	if hits := residueHits(files, m); len(hits) > 0 {
		return fmt.Errorf("residue remains after omitting %s:\n%s", m.Name, strings.Join(hits, "\n"))
	}
	return nil
}

func ownedPath(m *ModuleManifest, path string) bool {
	path = filepath.ToSlash(path)
	for _, p := range m.Paths {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if path == p || strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

func omitAllowlisted(relative string) bool {
	prefixes := []string{
		"docs/",
		"lib/foundation/modules/",
		"bin/foundation-modules",
		"test/lib/foundation/modules_test.rb",
		"PROVENANCE.md",
		"SPEC.md",
	}
	if relative == "README.md" {
		// README is rewritten wholesale after omit; residue there is OK during omit.
		return true
	}
	for _, p := range prefixes {
		if relative == p || strings.HasPrefix(relative, p) {
			return true
		}
	}
	return false
}

func stripModuleMarkers(source, name string) string {
	text := source
	text = stripCommentBlocks(text, name)
	text = collapseTaggedConditionals(text, name)
	return text
}

func stripCommentBlocks(text, name string) string {
	n := regexp.QuoteMeta(name)
	patterns := []*regexp.Regexp{
		// ERB
		regexp.MustCompile(`(?m)^[ \t]*<%#[ \t]*foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n(?s:.*?)^[ \t]*<%#[ \t]*/foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n?`),
		// CSS
		regexp.MustCompile(`(?m)^[ \t]*/\*[ \t]*foundation:module[ \t]+` + n + `[ \t]*\*/[ \t]*\n(?s:.*?)^[ \t]*/\*[ \t]*/foundation:module[ \t]+` + n + `[ \t]*\*/[ \t]*\n?`),
		// Hash comments
		regexp.MustCompile(`(?m)^[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*\n(?s:.*?)^[ \t]*#[ \t]*/foundation:module[ \t]+` + n + `[ \t]*\n?`),
	}
	for _, re := range patterns {
		text = re.ReplaceAllString(text, "")
	}
	return text
}

func collapseTaggedConditionals(text, name string) string {
	n := regexp.QuoteMeta(name)
	// ERB if/else/end → else body
	erbElse := regexp.MustCompile(`(?m)^[ \t]*<%[ \t]*if\b[^%]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n(?s:.*?)^[ \t]*<%[ \t]*else[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n((?s:.*?))^[ \t]*<%[ \t]*end[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n?`)
	text = erbElse.ReplaceAllString(text, "$1")
	// ERB if/end (no else) → remove
	erbIf := regexp.MustCompile(`(?m)^[ \t]*<%[ \t]*if\b[^%]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n(?s:.*?)^[ \t]*<%[ \t]*end[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*%>[ \t]*\n?`)
	text = erbIf.ReplaceAllString(text, "")
	// Ruby if/else/end
	rubyElse := regexp.MustCompile(`(?m)^[ \t]*if\b.*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*\n(?s:.*?)^[ \t]*else[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*\n((?s:.*?))^[ \t]*end[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*\n?`)
	text = rubyElse.ReplaceAllString(text, "$1")
	// Ruby if/end
	rubyIf := regexp.MustCompile(`(?m)^[ \t]*if\b.*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*\n(?s:.*?)^[ \t]*end[ \t]*#[ \t]*foundation:module[ \t]+` + n + `[ \t]*\n?`)
	text = rubyIf.ReplaceAllString(text, "")
	return text
}

func stripCSSPrefixLines(text string, patterns []string) string {
	var prefixes []string
	for _, p := range patterns {
		if strings.HasPrefix(p, ".") {
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		return text
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		skip := false
		for _, p := range prefixes {
			if strings.Contains(line, p) {
				skip = true
				break
			}
		}
		if !skip {
			b.WriteString(line)
		}
	}
	return b.String()
}

func stripFoundationYMLKeys(text string, keys []string) string {
	updated := text
	for _, key := range keys {
		re := regexp.MustCompile(`(?m)(?:^[ \t]*#[^\n]*\n)*^[ \t]*` + regexp.QuoteMeta(key) + `:[^\n]*\n(?:(?:^[ \t]*#[^\n]*\n)|(?:^[ \t]+-[^\n]*\n))*`)
		updated = re.ReplaceAllString(updated, "")
	}
	for strings.Contains(updated, "\n\n\n") {
		updated = strings.ReplaceAll(updated, "\n\n\n", "\n\n")
	}
	return updated
}

func stripSchemaTables(text string, prefixes []string) string {
	updated := text
	for _, prefix := range prefixes {
		p := regexp.QuoteMeta(prefix)
		create := regexp.MustCompile(`(?m)^  create_table "` + p + `[^"]*", force: :cascade do \|t\|(?s:.*?)^  end\n+`)
		updated = create.ReplaceAllString(updated, "")
		fk1 := regexp.MustCompile(`(?m)^  add_foreign_key "` + p + `[^"]*".*\n`)
		updated = fk1.ReplaceAllString(updated, "")
		fk2 := regexp.MustCompile(`(?m)^  add_foreign_key "[^"]*", "` + p + `[^"]*".*\n`)
		updated = fk2.ReplaceAllString(updated, "")
	}
	return updated
}

func residueHits(files map[string]string, m *ModuleManifest) []string {
	var hits []string
	marker := regexp.MustCompile(`foundation:module[ \t]+` + regexp.QuoteMeta(m.Name) + `\b`)
	var compiled []*regexp.Regexp
	for _, p := range m.ResiduePatterns {
		compiled = append(compiled, regexp.MustCompile(regexp.QuoteMeta(p)))
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if omitAllowlisted(path) {
			continue
		}
		// Only scan roots the foundation scans.
		if !underOmitScanRoot(path) {
			continue
		}
		content := files[path]
		if marker.MatchString(content) {
			hits = append(hits, path+": leftover module marker")
		}
		for i, re := range compiled {
			if re.MatchString(content) {
				hits = append(hits, fmt.Sprintf("%s: matches %q", path, m.ResiduePatterns[i]))
			}
		}
	}
	return hits
}

func underOmitScanRoot(path string) bool {
	if path == "README.md" {
		return true
	}
	for _, root := range []string{"app/", "bin/", "config/", "db/", "docs/", "lib/", "script/", "test/"} {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
