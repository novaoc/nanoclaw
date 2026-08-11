package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// appSpecRepoPath is where the validated specification is committed inside a
// generated application. Generation is a one-way fork, so this file is the
// durable record of intent the customer owns and may edit.
const appSpecRepoPath = "docs/APP_SPEC.json"

// AppSpec is the structured application specification Vela produces before
// writing product code. It is filled via the app_spec tool (never free text)
// and kept small enough to re-inject on every build turn.
type AppSpec struct {
	Name         string            `json:"name"`
	Purpose      string            `json:"purpose"`
	Actors       []string          `json:"actors"`
	Entities     []SpecEntity      `json:"entities"`
	Workflows    []SpecWorkflow    `json:"workflows"`
	Modules      []string          `json:"modules"`
	Integrations []SpecIntegration `json:"integrations"`
	SeedDemo     string            `json:"seed_demo"`
}

// SpecEntity is a core domain object and its key relationships.
type SpecEntity struct {
	Name          string   `json:"name"`
	Relationships []string `json:"relationships"`
}

// SpecWorkflow is a primary thing a user actually does in the product.
type SpecWorkflow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SpecIntegration names an external system and whether the app needs a demo
// adapter (local simulator / fake) for Holodex previews.
type SpecIntegration struct {
	Name        string `json:"name"`
	DemoAdapter bool   `json:"demo_adapter"`
}

// loadDeclaredModules reads foundation module manifests from
// <root>/config/foundation/modules/*.yml at runtime. Returns name → summary.
// Never uses a hardcoded module list — the foundation checkout is source of truth.
func loadDeclaredModules(root string) (map[string]string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("foundation root is not configured (set VELA_FOUNDATION_ROOT)")
	}
	dir := filepath.Join(root, "config", "foundation", "modules")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading foundation modules at %s: %w", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
		}
		name, summary := parseModuleManifest(string(raw))
		if name == "" {
			return nil, fmt.Errorf("module manifest %s has no name:", e.Name())
		}
		out[name] = summary
	}
	return out, nil
}

// parseModuleManifest extracts top-level name and summary from a minimal YAML
// manifest. Only the keys we need — no full YAML dependency. Indented keys are
// ignored so nested maps cannot shadow the module name.
func parseModuleManifest(content string) (name, summary string) {
	for _, raw := range strings.Split(content, "\n") {
		if len(raw) == 0 || raw[0] == ' ' || raw[0] == '\t' || raw[0] == '#' {
			continue
		}
		line := strings.TrimSpace(raw)
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "name":
			if name == "" {
				name = val
			}
		case "summary":
			if summary == "" {
				summary = val
			}
		}
	}
	return name, summary
}

// ValidateAppSpec rejects specs the model must fix before the build proceeds.
// known is the set of module names declared by the foundation (from loadDeclaredModules).
func ValidateAppSpec(spec *AppSpec, known map[string]string) error {
	if spec == nil {
		return fmt.Errorf("specification is required")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(spec.Purpose) == "" {
		return fmt.Errorf("purpose is required")
	}
	if len(spec.Entities) == 0 {
		return fmt.Errorf("entities must be non-empty — name at least one core domain object")
	}
	for i, e := range spec.Entities {
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("entities[%d].name is required", i)
		}
	}
	if len(spec.Workflows) == 0 {
		return fmt.Errorf("workflows must be non-empty — name at least one primary user workflow")
	}
	for i, w := range spec.Workflows {
		if strings.TrimSpace(w.Name) == "" {
			return fmt.Errorf("workflows[%d].name is required", i)
		}
	}
	if known == nil {
		known = map[string]string{}
	}
	var unknown []string
	for _, m := range spec.Modules {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := known[m]; !ok {
			unknown = append(unknown, m)
		}
	}
	if len(unknown) > 0 {
		declared := make([]string, 0, len(known))
		for k := range known {
			declared = append(declared, k)
		}
		sort.Strings(declared)
		return fmt.Errorf("unknown module %s — foundation declares: %s",
			strings.Join(unknown, ", "),
			orNoneList(declared))
	}
	return nil
}

func orNoneList(ss []string) string {
	if len(ss) == 0 {
		return "(none)"
	}
	return strings.Join(ss, ", ")
}

// MarshalAppSpec encodes a compact, stable JSON form for the repo and context.
func MarshalAppSpec(spec *AppSpec) ([]byte, error) {
	if spec == nil {
		return nil, fmt.Errorf("specification is required")
	}
	return json.MarshalIndent(spec, "", "  ")
}

// UnmarshalAppSpec decodes a previously written specification.
func UnmarshalAppSpec(data []byte) (*AppSpec, error) {
	var s AppSpec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AmendAppSpec overlays non-empty fields from patch onto base and returns a new
// spec. Slice fields in the patch replace the base slice when non-nil (including
// empty slices, so the model can clear a list deliberately). Scalar strings
// replace only when non-empty.
func AmendAppSpec(base, patch *AppSpec) *AppSpec {
	if base == nil {
		base = &AppSpec{}
	}
	out := *base
	// deep-ish copy slices so we don't mutate the stored base on failed validate
	out.Actors = append([]string(nil), base.Actors...)
	out.Entities = append([]SpecEntity(nil), base.Entities...)
	out.Workflows = append([]SpecWorkflow(nil), base.Workflows...)
	out.Modules = append([]string(nil), base.Modules...)
	out.Integrations = append([]SpecIntegration(nil), base.Integrations...)
	if patch == nil {
		return &out
	}
	if strings.TrimSpace(patch.Name) != "" {
		out.Name = patch.Name
	}
	if strings.TrimSpace(patch.Purpose) != "" {
		out.Purpose = patch.Purpose
	}
	if strings.TrimSpace(patch.SeedDemo) != "" {
		out.SeedDemo = patch.SeedDemo
	}
	if patch.Actors != nil {
		out.Actors = append([]string(nil), patch.Actors...)
	}
	if patch.Entities != nil {
		out.Entities = append([]SpecEntity(nil), patch.Entities...)
	}
	if patch.Workflows != nil {
		out.Workflows = append([]SpecWorkflow(nil), patch.Workflows...)
	}
	if patch.Modules != nil {
		out.Modules = append([]string(nil), patch.Modules...)
	}
	if patch.Integrations != nil {
		out.Integrations = append([]SpecIntegration(nil), patch.Integrations...)
	}
	return &out
}

// appSpecArgs is the tool payload. Nested objects match the JSON schema the
// model fills; we never parse free-text prose into a spec.
type appSpecArgs struct {
	Action       string            `json:"action"` // set | amend | show
	Repo         string            `json:"repo"`   // optional: commit validated spec here
	Name         string            `json:"name"`
	Purpose      string            `json:"purpose"`
	Actors       []string          `json:"actors"`
	Entities     []SpecEntity      `json:"entities"`
	Workflows    []SpecWorkflow    `json:"workflows"`
	Modules      []string          `json:"modules"`
	Integrations []SpecIntegration `json:"integrations"`
	SeedDemo     string            `json:"seed_demo"`
}

func (a appSpecArgs) asSpec() *AppSpec {
	return &AppSpec{
		Name:         a.Name,
		Purpose:      a.Purpose,
		Actors:       a.Actors,
		Entities:     a.Entities,
		Workflows:    a.Workflows,
		Modules:      a.Modules,
		Integrations: a.Integrations,
		SeedDemo:     a.SeedDemo,
	}
}

func (tc *ToolCtx) runAppSpec(raw string) string {
	var a appSpecArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "app_spec error: bad arguments: " + err.Error()
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		action = "set"
	}

	known, err := loadDeclaredModules(tc.cfg.FoundationRoot)
	if err != nil {
		// show can still report in-memory state without the foundation
		if action != "show" {
			return "app_spec error: " + err.Error()
		}
		known = map[string]string{}
	}

	switch action {
	case "show":
		if tc.appSpec == nil {
			return "no application specification set yet"
		}
		b, err := MarshalAppSpec(tc.appSpec)
		if err != nil {
			return "app_spec error: " + err.Error()
		}
		return fmt.Sprintf("current specification (%s):\n%s", appSpecRepoPath, b)

	case "set":
		spec := a.asSpec()
		if err := ValidateAppSpec(spec, known); err != nil {
			return "app_spec rejected: " + err.Error() + ". Fix the specification and call app_spec again."
		}
		tc.appSpec = spec
		return tc.persistAppSpec(a.Repo, "set application specification")

	case "amend":
		if tc.appSpec == nil {
			return "app_spec error: nothing to amend — call action=set first"
		}
		next := AmendAppSpec(tc.appSpec, a.asSpec())
		if err := ValidateAppSpec(next, known); err != nil {
			return "app_spec amendment rejected: " + err.Error() + ". Current specification is unchanged. Fix the amendment and try again."
		}
		tc.appSpec = next
		return tc.persistAppSpec(a.Repo, "amend application specification")

	default:
		return "app_spec error: unknown action " + a.Action + " (use set|amend|show)"
	}
}

func (tc *ToolCtx) persistAppSpec(repo, message string) string {
	b, err := MarshalAppSpec(tc.appSpec)
	if err != nil {
		return "app_spec error: " + err.Error()
	}
	body := string(b)

	var parts []string
	parts = append(parts, "specification accepted")

	// Always keep a workspace copy when the coder shell is available so the
	// build can re-read it without GitHub.
	if tc.cfg.CodeEnabled() && tc.isCoder() {
		if out := tc.writeWorkspaceFile(appSpecRepoPath, body+"\n"); strings.HasPrefix(out, "wrote") {
			parts = append(parts, "wrote workspace "+appSpecRepoPath)
		}
	}

	repo = strings.TrimSpace(repo)
	if repo != "" {
		if !tc.cfg.GithubEnabled() {
			parts = append(parts, "repo commit skipped (GitHub not configured)")
		} else if !tc.cfg.RepoAllowed(tc.authorID) {
			parts = append(parts, "repo commit skipped (not on VELA_REPO_USERS)")
		} else if gh := newGH(tc.cfg); gh == nil {
			parts = append(parts, "repo commit skipped (no token)")
		} else {
			tc.usedCode = true
			out := gh.putFile(repo, appSpecRepoPath, body+"\n", message, "")
			parts = append(parts, out)
		}
	} else {
		parts = append(parts, fmt.Sprintf("commit this JSON to %s in the generated repo (put_file) so the customer owns the intent record", appSpecRepoPath))
	}

	parts = append(parts, "spec:\n"+body)
	return strings.Join(parts, "\n")
}

// declaredModulesSummary is a short, model-facing list for tool descriptions.
func declaredModulesSummary(root string) string {
	m, err := loadDeclaredModules(root)
	if err != nil || len(m) == 0 {
		return "none declared (check VELA_FOUNDATION_ROOT)"
	}
	names := make([]string, 0, len(m))
	for n, sum := range m {
		if sum != "" {
			names = append(names, n+" ("+sum+")")
		} else {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "; ")
}
