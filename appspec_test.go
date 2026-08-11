package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModuleFixture builds a minimal foundation modules directory for tests.
// Validation must read this tree at runtime — not a hardcoded allowlist.
func writeModuleFixture(t *testing.T, names map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "config", "foundation", "modules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, summary := range names {
		body := "name: " + name + "\nsummary: " + summary + "\ndefault: included\npaths: []\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func validSpec() *AppSpec {
	return &AppSpec{
		Name:    "Acme Board",
		Purpose: "Guest-first digital board-game storefront",
		Actors:  []string{"guest", "owner"},
		Entities: []SpecEntity{
			{Name: "Product", Relationships: []string{"has many OrderLine"}},
			{Name: "Order", Relationships: []string{"belongs to User optionally", "has many OrderLine"}},
		},
		Workflows: []SpecWorkflow{
			{Name: "browse catalog", Description: "Guest browses products"},
			{Name: "checkout", Description: "Guest pays and receives a receipt link"},
		},
		Modules: []string{"storefront"},
		Integrations: []SpecIntegration{
			{Name: "stripe", DemoAdapter: true},
		},
		SeedDemo: "Five products across two categories and one completed guest order",
	}
}

func TestAppSpecRoundTrip(t *testing.T) {
	root := writeModuleFixture(t, map[string]string{
		"storefront": "Guest-first digital catalog",
	})
	known, err := loadDeclaredModules(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	if err := ValidateAppSpec(spec, known); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	raw, err := MarshalAppSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalAppSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != spec.Name || got.Purpose != spec.Purpose || got.SeedDemo != spec.SeedDemo {
		t.Fatalf("scalar mismatch: %+v", got)
	}
	if len(got.Entities) != 2 || got.Entities[0].Name != "Product" {
		t.Fatalf("entities: %+v", got.Entities)
	}
	if len(got.Workflows) != 2 || got.Workflows[1].Name != "checkout" {
		t.Fatalf("workflows: %+v", got.Workflows)
	}
	if len(got.Modules) != 1 || got.Modules[0] != "storefront" {
		t.Fatalf("modules: %+v", got.Modules)
	}
	if len(got.Integrations) != 1 || !got.Integrations[0].DemoAdapter {
		t.Fatalf("integrations: %+v", got.Integrations)
	}
	// JSON must stay small and structured (not prose blob)
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "purpose", "entities", "workflows", "modules"} {
		if _, ok := probe[key]; !ok {
			t.Fatalf("missing key %q in marshaled spec", key)
		}
	}
}

func TestAppSpecValidationTable(t *testing.T) {
	root := writeModuleFixture(t, map[string]string{
		"storefront": "Guest-first digital catalog",
	})
	known, err := loadDeclaredModules(root)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		mutate  func(*AppSpec)
		wantErr string
	}{
		{
			name: "valid accepted",
			mutate: func(*AppSpec) {},
		},
		{
			name: "unknown module rejected by name",
			mutate: func(s *AppSpec) {
				s.Modules = []string{"storefront", "crm"}
			},
			wantErr: "unknown module crm",
		},
		{
			name: "empty entities rejected",
			mutate: func(s *AppSpec) {
				s.Entities = nil
			},
			wantErr: "entities must be non-empty",
		},
		{
			name: "empty workflows rejected",
			mutate: func(s *AppSpec) {
				s.Workflows = nil
			},
			wantErr: "workflows must be non-empty",
		},
		{
			name: "empty entity name rejected",
			mutate: func(s *AppSpec) {
				s.Entities = []SpecEntity{{Name: ""}}
			},
			wantErr: "entities[0].name",
		},
		{
			name: "missing name rejected",
			mutate: func(s *AppSpec) {
				s.Name = "  "
			},
			wantErr: "name is required",
		},
		{
			name: "no modules is ok when only core is needed",
			mutate: func(s *AppSpec) {
				s.Modules = nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(s)
			err := ValidateAppSpec(s, known)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// Prove module validation reads the fixture tree, not a baked-in constant:
// a name that would be plausible but is absent from THIS foundation fails.
func TestAppSpecModulesComeFromFoundationFixture(t *testing.T) {
	// Fixture deliberately omits storefront — only "billing_extra" exists.
	root := writeModuleFixture(t, map[string]string{
		"billing_extra": "Optional billing add-on",
	})
	known, err := loadDeclaredModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known["storefront"]; ok {
		t.Fatal("fixture must not declare storefront")
	}
	if _, ok := known["billing_extra"]; !ok {
		t.Fatal("fixture should declare billing_extra")
	}

	spec := validSpec()
	spec.Modules = []string{"storefront"} // absent from this foundation
	err = ValidateAppSpec(spec, known)
	if err == nil || !strings.Contains(err.Error(), "unknown module storefront") {
		t.Fatalf("expected storefront rejected from fixture, got %v", err)
	}
	if !strings.Contains(err.Error(), "billing_extra") {
		t.Fatalf("error should list declared modules, got %v", err)
	}

	// Same code accepts the module that IS in the fixture.
	spec.Modules = []string{"billing_extra"}
	if err := ValidateAppSpec(spec, known); err != nil {
		t.Fatalf("fixture module should pass: %v", err)
	}
}

func TestAppSpecAmendmentRevalidates(t *testing.T) {
	root := writeModuleFixture(t, map[string]string{
		"storefront": "Guest-first digital catalog",
	})
	known, err := loadDeclaredModules(root)
	if err != nil {
		t.Fatal(err)
	}
	base := validSpec()
	if err := ValidateAppSpec(base, known); err != nil {
		t.Fatal(err)
	}

	// Valid amendment: add an actor and tweak purpose.
	next := AmendAppSpec(base, &AppSpec{
		Purpose: "Board-game marketplace for guests",
		Actors:  []string{"guest", "owner", "fulfillment"},
	})
	if err := ValidateAppSpec(next, known); err != nil {
		t.Fatalf("valid amendment rejected: %v", err)
	}
	if next.Purpose != "Board-game marketplace for guests" || len(next.Actors) != 3 {
		t.Fatalf("amendment not applied: %+v", next)
	}
	// Base must be unchanged (amend returns a copy).
	if base.Purpose != "Guest-first digital board-game storefront" {
		t.Fatal("base was mutated")
	}

	// Invalid amendment: wipe entities — must fail validation.
	bad := AmendAppSpec(base, &AppSpec{Entities: []SpecEntity{}})
	err = ValidateAppSpec(bad, known)
	if err == nil || !strings.Contains(err.Error(), "entities must be non-empty") {
		t.Fatalf("empty entities amendment should fail, got %v", err)
	}

	// Invalid amendment: unknown module.
	bad = AmendAppSpec(base, &AppSpec{Modules: []string{"nope_module"}})
	err = ValidateAppSpec(bad, known)
	if err == nil || !strings.Contains(err.Error(), "unknown module nope_module") {
		t.Fatalf("unknown module amendment should fail, got %v", err)
	}
}

func TestAppSpecToolSetAmendShow(t *testing.T) {
	root := writeModuleFixture(t, map[string]string{
		"storefront": "Guest-first digital catalog",
	})
	cfg := testCfg(t)
	cfg.FoundationRoot = root
	tc := &ToolCtx{cfg: cfg, authorID: "u"}

	// Empty entities rejected through the tool with a fix-it message.
	out := tc.Run("app_spec", `{
		"action":"set",
		"name":"X","purpose":"Y",
		"entities":[],
		"workflows":[{"name":"w"}]
	}`)
	if !strings.Contains(out, "app_spec rejected") || !strings.Contains(out, "entities must be non-empty") {
		t.Fatalf("expected entity rejection, got %s", out)
	}
	if tc.appSpec != nil {
		t.Fatal("rejected set must not store a spec")
	}

	// Unknown module names the module.
	out = tc.Run("app_spec", `{
		"action":"set",
		"name":"X","purpose":"Y",
		"entities":[{"name":"E"}],
		"workflows":[{"name":"w"}],
		"modules":["ghost"]
	}`)
	if !strings.Contains(out, "unknown module ghost") {
		t.Fatalf("expected unknown module, got %s", out)
	}

	// Valid set.
	out = tc.Run("app_spec", `{
		"action":"set",
		"name":"Acme","purpose":"Sell boards",
		"actors":["guest"],
		"entities":[{"name":"Product","relationships":["has many variants"]}],
		"workflows":[{"name":"buy","description":"guest checkout"}],
		"modules":["storefront"],
		"integrations":[{"name":"stripe","demo_adapter":true}],
		"seed_demo":"three products"
	}`)
	if !strings.Contains(out, "specification accepted") {
		t.Fatalf("expected accept, got %s", out)
	}
	if tc.appSpec == nil || tc.appSpec.Name != "Acme" {
		t.Fatalf("spec not stored: %+v", tc.appSpec)
	}

	// Amend that would invalidate is rejected; prior spec stays.
	out = tc.Run("app_spec", `{"action":"amend","modules":["not_real"]}`)
	if !strings.Contains(out, "amendment rejected") || !strings.Contains(out, "not_real") {
		t.Fatalf("expected amend rejection, got %s", out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Fatalf("should say prior spec unchanged: %s", out)
	}
	if tc.appSpec.Modules[0] != "storefront" {
		t.Fatalf("prior modules overwritten on failed amend: %+v", tc.appSpec.Modules)
	}

	// Valid amend.
	out = tc.Run("app_spec", `{"action":"amend","purpose":"Sell boards and dice"}`)
	if !strings.Contains(out, "specification accepted") {
		t.Fatalf("expected amend accept, got %s", out)
	}
	if tc.appSpec.Purpose != "Sell boards and dice" {
		t.Fatalf("purpose not amended: %q", tc.appSpec.Purpose)
	}

	out = tc.Run("app_spec", `{"action":"show"}`)
	if !strings.Contains(out, "Sell boards and dice") || !strings.Contains(out, appSpecRepoPath) {
		t.Fatalf("show failed: %s", out)
	}
}

func TestAppSpecToolOfferedWhenFoundationConfigured(t *testing.T) {
	cfg := testCfg(t)
	cfg.FoundationRoot = writeModuleFixture(t, map[string]string{"storefront": "shop"})
	found := false
	for _, d := range toolDefs(cfg) {
		if d.Function.Name == "app_spec" {
			found = true
			if !strings.Contains(d.Function.Description, "storefront") {
				t.Fatalf("tool description should list declared modules: %s", d.Function.Description)
			}
			if !strings.Contains(d.Function.Description, appSpecRepoPath) {
				t.Fatalf("tool description should name commit path: %s", d.Function.Description)
			}
		}
	}
	if !found {
		t.Fatal("app_spec tool missing when foundation root is set")
	}
}

func TestLoadDeclaredModulesEmptyRoot(t *testing.T) {
	_, err := loadDeclaredModules("")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("got %v", err)
	}
}

func TestParseModuleManifestIgnoresNested(t *testing.T) {
	raw := "# comment\nname: storefront\nsummary: Shop\npaths:\n  - app/models\n  name: nested\n"
	name, summary := parseModuleManifest(raw)
	if name != "storefront" || summary != "Shop" {
		t.Fatalf("got %q %q", name, summary)
	}
}
