package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleFoundationYML() string {
	return `shared:
  application_name: "Application"
  logo_url: ""
  brand_seed_color: "#6750A4"
  default_page_title: "Welcome"
  default_page_description: "A production-ready Rails application."
  default_og_image_url: ""
  social_links: {}
  support_email: "support@example.com"
  legal_email: "legal@example.com"
  domain: "example.com"
  storefront_enabled: true
  storefront_fulfillment_mode: "digital"
  storefront_commerce_legal_reviewed: false
  storefront_external_image_hosts: []
`
}

func sampleREADME() string {
	return `<!-- foundation:identity -->
# Application

A production-ready Rails application.

- Site: https://example.com
- Support: support@example.com
<!-- /foundation:identity -->

The block above is the product identity.

## What this is

A Rails 8.1 starter template. This documents the foundation, not the product.
`
}

func writeFullModuleFixture(t *testing.T, modules map[string]*ModuleManifest) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "config", "foundation", "modules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, m := range modules {
		var b strings.Builder
		fmt.Fprintf(&b, "name: %s\n", name)
		fmt.Fprintf(&b, "summary: %s\n", m.Summary)
		b.WriteString("default: included\npaths:\n")
		for _, p := range m.Paths {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		if len(m.TablePrefixes) > 0 {
			b.WriteString("table_prefixes:\n")
			for _, p := range m.TablePrefixes {
				fmt.Fprintf(&b, "  - %s\n", p)
			}
		}
		if len(m.ConfigKeys) > 0 {
			b.WriteString("config_keys:\n")
			for _, p := range m.ConfigKeys {
				fmt.Fprintf(&b, "  - %s\n", p)
			}
		} else {
			b.WriteString("config_keys: []\n")
		}
		if len(m.ResiduePatterns) > 0 {
			b.WriteString("residue_patterns:\n")
			for _, p := range m.ResiduePatterns {
				fmt.Fprintf(&b, "  - %q\n", p)
			}
		}
		if len(m.DependsOn) > 0 {
			b.WriteString("depends_on:\n")
			for _, p := range m.DependsOn {
				fmt.Fprintf(&b, "  - %s\n", p)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yml"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fixtureModules() map[string]*ModuleManifest {
	return map[string]*ModuleManifest{
		"storefront": {
			Name:    "storefront",
			Summary: "Shop",
			Paths: []string{
				"app/models/foundation/storefront",
				"app/controllers/foundation/storefront",
				"lib/foundation/demo_seeds.rb",
			},
			TablePrefixes:   []string{"storefront_"},
			ConfigKeys:      []string{"storefront_enabled", "storefront_fulfillment_mode"},
			ResiduePatterns: []string{"Foundation::Storefront", "storefront_enabled", "storefront_cart_path"},
		},
		"crm": {
			Name:    "crm",
			Summary: "CRM",
			Paths: []string{
				"app/models/foundation/crm",
				"app/controllers/foundation/crm",
			},
			TablePrefixes:   []string{"crm_"},
			ConfigKeys:      nil,
			ResiduePatterns: []string{"Foundation::Crm", "crm_contacts_path"},
		},
	}
}

func fixtureRepoFiles() map[string]string {
	return map[string]string{
		foundationYMLPath: sampleFoundationYML(),
		readmePath:        sampleREADME(),
		"db/schema.rb": `ActiveRecord::Schema[8.1].define(version: 1) do
  create_table "users", force: :cascade do |t|
    t.string "email"
  end

  create_table "storefront_products", force: :cascade do |t|
    t.string "name"
  end

  create_table "crm_contacts", force: :cascade do |t|
    t.string "name"
  end

  add_foreign_key "storefront_products", "users"
end
`,
		"config/routes.rb": `# frozen_string_literal: true
Rails.application.routes.draw do
  # foundation:module storefront
  namespace :storefront do
    resources :products
  end
  # /foundation:module storefront

  # foundation:module crm
  namespace :crm do
    resources :contacts
  end
  # /foundation:module crm

  root "home#index"
end
`,
		"app/models/user.rb": `class User < ApplicationRecord
  # foundation:module storefront
  has_many :storefront_orders, class_name: "Foundation::Storefront::Order"
  # /foundation:module storefront
end
`,
		"app/models/foundation/storefront/product.rb":  "module Foundation; module Storefront; class Product; end; end; end\n",
		"app/controllers/foundation/storefront/c.rb":  "class Foundation::Storefront::C < ApplicationController; end\n",
		"lib/foundation/demo_seeds.rb":                "Foundation::Storefront\n",
		"app/models/foundation/crm/contact.rb":         "module Foundation; module Crm; class Contact; end; end; end\n",
		"app/controllers/foundation/crm/contacts.rb":   "class Foundation::Crm::ContactsController; end\n",
		"config/foundation/modules/storefront.yml":    "name: storefront\n",
		"config/foundation/modules/crm.yml":           "name: crm\n",
		"app/views/layouts/application.html.erb": `<%# foundation:module storefront %>
  <%= link_to "Shop", storefront_cart_path %>
<%# /foundation:module storefront %>
<% if Foundation.storefront_enabled? # foundation:module storefront %>
  <%= link_to "Shop", "#" %>
<% else # foundation:module storefront %>
  <%= link_to "Pricing", pricing_path %>
<% end # foundation:module storefront %>
`,
	}
}

func TestIdentityStampFoundationAndREADME(t *testing.T) {
	id := AppIdentity{
		ApplicationName: "Driftline Coffee",
		Description:     "Guest coffee preorders.",
		Domain:          "driftline-coffee.demo.holode.xyz",
		SupportEmail:    "support@driftline-coffee.demo.holode.xyz",
	}
	yml, err := stampFoundationYML(sampleFoundationYML(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`application_name: "Driftline Coffee"`,
		`default_page_description: "Guest coffee preorders."`,
		`domain: "driftline-coffee.demo.holode.xyz"`,
		`support_email: "support@driftline-coffee.demo.holode.xyz"`,
	} {
		if !strings.Contains(yml, want) {
			t.Fatalf("foundation.yml missing %q in:\n%s", want, yml)
		}
	}
	if strings.Contains(yml, `domain: "example.com"`) {
		t.Fatal("example.com domain left in place")
	}

	readme, err := stampREADMEIdentity(sampleREADME(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readme, identityStart) || !strings.Contains(readme, identityEnd) {
		t.Fatal("identity markers must be preserved")
	}
	if !strings.Contains(readme, "# Driftline Coffee") {
		t.Fatal("identity title missing")
	}
	if !strings.Contains(readme, "https://driftline-coffee.demo.holode.xyz") {
		t.Fatal("identity site missing")
	}
	if strings.Contains(readme, "example.com") {
		// body may still mention template until full rewrite; identity block must not
		blockStart := strings.Index(readme, identityStart)
		blockEnd := strings.Index(readme, identityEnd)
		block := readme[blockStart : blockEnd+len(identityEnd)]
		if strings.Contains(block, "example.com") {
			t.Fatalf("identity block still has example.com:\n%s", block)
		}
	}
}

func TestBuildAppREADMENotFoundation(t *testing.T) {
	spec := &AppSpec{
		Name:     "Driftline Coffee",
		Purpose:  "Guests preorder coffee for pickup",
		Actors:   []string{"guest", "barista"},
		Entities: []SpecEntity{{Name: "Order"}},
		Workflows: []SpecWorkflow{
			{Name: "preorder", Description: "Guest places a pickup order"},
		},
		Modules:  []string{"storefront"},
		SeedDemo: "Three drinks and one completed pickup order",
	}
	id := identityFromSpec(spec, "driftline-coffee", "https://demo.holode.xyz")
	body := buildAppREADME(spec, id)
	if !strings.Contains(body, identityStart) || !strings.Contains(body, identityEnd) {
		t.Fatal("markers required")
	}
	if !strings.Contains(body, "Guests preorder coffee") {
		t.Fatal("purpose missing")
	}
	if !strings.Contains(body, "preorder") || !strings.Contains(body, "storefront") {
		t.Fatal("features/modules missing")
	}
	if strings.Contains(body, "starter template") || strings.Contains(body, "one-way fork") {
		t.Fatal("must not describe the foundation template")
	}
	if id.Domain != "driftline-coffee.demo.holode.xyz" {
		t.Fatalf("domain=%q", id.Domain)
	}
	if strings.Contains(id.Domain, "example.com") {
		t.Fatal("domain must not be example.com")
	}
}

func TestOmitAllOptionalModules(t *testing.T) {
	declared := fixtureModules()
	files := fixtureRepoFiles()
	spec := &AppSpec{
		Name:      "Landing",
		Purpose:   "A simple landing page",
		Entities:  []SpecEntity{{Name: "Page"}},
		Workflows: []SpecWorkflow{{Name: "view"}},
		Modules:   nil, // keep none
	}
	id := identityFromSpec(spec, "landing", "https://demo.holode.xyz")
	plan, err := planShape(files, spec, id, declared)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Omitted) != 2 || len(plan.Kept) != 0 {
		t.Fatalf("omitted=%v kept=%v", plan.Omitted, plan.Kept)
	}
	// Owned paths deleted
	for _, p := range []string{
		"app/models/foundation/storefront/product.rb",
		"app/models/foundation/crm/contact.rb",
		"config/foundation/modules/storefront.yml",
		"config/foundation/modules/crm.yml",
		"lib/foundation/demo_seeds.rb",
	} {
		if _, ok := plan.Writes[p]; ok {
			t.Fatalf("deleted path should not be a write: %s", p)
		}
		found := false
		for _, d := range plan.Deletes {
			if d == p {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected delete %s; deletes=%v", p, plan.Deletes)
		}
	}
	// Routes stripped of both modules
	routes := plan.Writes["config/routes.rb"]
	if routes == "" {
		t.Fatal("routes should be rewritten")
	}
	if strings.Contains(routes, "storefront") || strings.Contains(routes, "crm") {
		t.Fatalf("routes still mention omitted modules:\n%s", routes)
	}
	// foundation.yml identity stamped + storefront keys gone
	yml := plan.Writes[foundationYMLPath]
	if !strings.Contains(yml, `application_name: "Landing"`) {
		t.Fatalf("identity not stamped:\n%s", yml)
	}
	if strings.Contains(yml, "storefront_enabled") || strings.Contains(yml, "example.com") {
		t.Fatalf("yml not cleaned:\n%s", yml)
	}
	// schema scrubbed
	schema := plan.Writes["db/schema.rb"]
	if strings.Contains(schema, "storefront_") || strings.Contains(schema, "crm_") {
		t.Fatalf("schema still has module tables:\n%s", schema)
	}
	// README is the app README
	if !strings.Contains(plan.Writes[readmePath], "simple landing page") {
		t.Fatalf("app README missing purpose:\n%s", plan.Writes[readmePath])
	}
	// Layout: storefront block removed; else branch kept
	layout := plan.Writes["app/views/layouts/application.html.erb"]
	if strings.Contains(layout, "storefront_cart_path") || strings.Contains(layout, "foundation:module") {
		t.Fatalf("layout not stripped:\n%s", layout)
	}
	if !strings.Contains(layout, "Pricing") {
		t.Fatalf("else branch should remain:\n%s", layout)
	}
}

func TestOmitKeepsSelectedModule(t *testing.T) {
	declared := fixtureModules()
	files := fixtureRepoFiles()
	spec := &AppSpec{
		Name:      "Shop",
		Purpose:   "Sell widgets",
		Entities:  []SpecEntity{{Name: "Product"}},
		Workflows: []SpecWorkflow{{Name: "buy"}},
		Modules:   []string{"storefront"},
	}
	id := identityFromSpec(spec, "shop", "")
	plan, err := planShape(files, spec, id, declared)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 1 || plan.Kept[0] != "storefront" {
		t.Fatalf("kept=%v", plan.Kept)
	}
	if len(plan.Omitted) != 1 || plan.Omitted[0] != "crm" {
		t.Fatalf("omitted=%v", plan.Omitted)
	}
	// storefront paths stay (not in Deletes)
	for _, p := range []string{
		"app/models/foundation/storefront/product.rb",
		"config/foundation/modules/storefront.yml",
	} {
		for _, d := range plan.Deletes {
			if d == p {
				t.Fatalf("must keep %s", p)
			}
		}
	}
	// crm gone
	for _, p := range []string{
		"app/models/foundation/crm/contact.rb",
		"config/foundation/modules/crm.yml",
	} {
		found := false
		for _, d := range plan.Deletes {
			if d == p {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected crm delete %s", p)
		}
	}
	routes := plan.Writes["config/routes.rb"]
	if !strings.Contains(routes, "storefront") {
		t.Fatal("storefront routes must remain")
	}
	if strings.Contains(routes, "crm") {
		t.Fatal("crm routes must be gone")
	}
	// storefront config keys remain when module kept
	yml := plan.Writes[foundationYMLPath]
	if !strings.Contains(yml, "storefront_enabled") {
		t.Fatalf("storefront keys should remain:\n%s", yml)
	}
}

func TestOmitUnknownModuleFailsLoudly(t *testing.T) {
	declared := fixtureModules()
	files := fixtureRepoFiles()
	spec := &AppSpec{
		Name:      "X",
		Purpose:   "Y",
		Entities:  []SpecEntity{{Name: "E"}},
		Workflows: []SpecWorkflow{{Name: "w"}},
		Modules:   []string{"storefront", "ghost_module"},
	}
	id := identityFromSpec(spec, "x", "")
	_, err := planShape(files, spec, id, declared)
	if err == nil || !strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("want unknown module error, got %v", err)
	}
	if !strings.Contains(err.Error(), "ghost_module") {
		t.Fatalf("should name the module: %v", err)
	}
}

func TestLoadFullModuleManifests(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	got, err := loadFullModuleManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d modules", len(got))
	}
	sf := got["storefront"]
	if sf == nil || len(sf.Paths) < 2 || sf.ConfigKeys[0] != "storefront_enabled" {
		t.Fatalf("storefront manifest: %+v", sf)
	}
}

// shapeStub holds GitHub API fixture state for create_rails_app + shape tests.
type shapeStub struct {
	repo        string
	files       map[string]string
	blobs       map[string]string // sha → content
	treeEntries []any
	postedTree  []any
	generated   bool
	treePosted  bool
	commitPosted bool
	refPatched  bool
}

func newShapeStub(repo string, files map[string]string) *shapeStub {
	s := &shapeStub{repo: repo, files: files, blobs: map[string]string{}}
	i := 0
	for path, content := range files {
		i++
		sha := fmt.Sprintf("sha-%d", i)
		s.blobs[sha] = content
		s.treeEntries = append(s.treeEntries, map[string]any{
			"path": path, "type": "blob", "sha": sha, "size": float64(len(content)),
		})
	}
	return s
}

func (s *shapeStub) handle(w http.ResponseWriter, r *http.Request, _ *ghTestEnv) {
	repoPrefix := "/repos/velaoc/" + s.repo
	switch {
	case r.URL.Path == "/user":
		writeJSON(w, 200, map[string]any{"login": "velaoc"})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/generate"):
		s.generated = true
		writeJSON(w, 201, map[string]any{
			"html_url": "https://github.com/velaoc/" + s.repo,
			"name":     s.repo,
		})
	case r.URL.Path == repoPrefix && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"default_branch": "main"})
	case strings.Contains(r.URL.Path, "/contents/config/foundation.yml") && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{
			"sha":     "yml-sha",
			"content": base64.StdEncoding.EncodeToString([]byte(s.files[foundationYMLPath])),
		})
	case strings.HasPrefix(r.URL.Path, repoPrefix+"/git/trees/"):
		writeJSON(w, 200, map[string]any{"sha": "tree-sha", "tree": s.treeEntries})
	case strings.HasPrefix(r.URL.Path, repoPrefix+"/git/blobs/"):
		sha := filepath.Base(r.URL.Path)
		content, ok := s.blobs[sha]
		if !ok {
			writeJSON(w, 404, map[string]any{"message": "nope"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"sha":     sha,
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		})
	// GitHub updates a ref at the PLURAL path. Accepting a PATCH on the
	// singular read path here is what let the 404 ship: the stub was more
	// forgiving than the API.
	case r.URL.Path == repoPrefix+"/git/refs/heads/main" && r.Method == http.MethodPatch:
		s.refPatched = true
		writeJSON(w, 200, map[string]any{"object": map[string]any{"sha": "new-commit"}})
	case r.URL.Path == repoPrefix+"/git/ref/heads/main":
		writeJSON(w, 200, map[string]any{"object": map[string]any{"sha": "commit-base"}})
	case r.URL.Path == repoPrefix+"/git/commits/commit-base":
		writeJSON(w, 200, map[string]any{"sha": "commit-base", "tree": map[string]any{"sha": "base-tree"}})
	case r.URL.Path == repoPrefix+"/git/trees" && r.Method == http.MethodPost:
		s.treePosted = true
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.postedTree, _ = body["tree"].([]any)
		writeJSON(w, 201, map[string]any{"sha": "new-tree"})
	case r.URL.Path == repoPrefix+"/git/commits" && r.Method == http.MethodPost:
		s.commitPosted = true
		writeJSON(w, 201, map[string]any{"sha": "new-commit"})
	default:
		writeJSON(w, 404, map[string]any{"message": "unhandled " + r.Method + " " + r.URL.Path})
	}
}

func (s *shapeStub) contentOf(path string) string {
	for _, raw := range s.postedTree {
		entry, _ := raw.(map[string]any)
		if p, _ := entry["path"].(string); p == path {
			c, _ := entry["content"].(string)
			return c
		}
	}
	return ""
}

func (s *shapeStub) deleted(path string) bool {
	for _, raw := range s.postedTree {
		entry, _ := raw.(map[string]any)
		if p, _ := entry["path"].(string); p == path {
			return entry["sha"] == nil
		}
	}
	return false
}

// createAndShape mirrors create_rails_app post-generate shaping on the stub client.
func createAndShape(g *ghClient, template, name, desc string, public bool, spec *AppSpec, cfg *Config) string {
	out := g.createFromTemplate(template, name, desc, public)
	if !strings.HasPrefix(out, "created Rails app") {
		return out
	}
	return out + "\n" + g.shapeGeneratedApp(name, spec, cfg)
}

func TestCreateRailsAppDoesNotRequireAppSpec(t *testing.T) {
	// Dispatch must not refuse create_rails_app when app_spec is unset —
	// shaping derives identity from the app name instead.
	cfg := testCfg(t)
	cfg.GitHubToken = "t"
	cfg.RailsTemplate = "velaoc/foundation"
	tc := &ToolCtx{cfg: cfg, authorID: "dev"}
	out := tc.runGithub(toolArgs{Action: "create_rails_app", Name: "demo"})
	if strings.Contains(out, "set app_spec before") {
		t.Fatalf("create_rails_app must not require app_spec, got %s", out)
	}
	// Without a reachable API it fails at generate, not at the app_spec gate.
	if strings.Contains(strings.ToLower(out), "app_spec") && strings.Contains(out, "before create") {
		t.Fatalf("unexpected app_spec gate: %s", out)
	}
}

func TestCreateRailsAppStampsIdentityWithAppSpec(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	stub := newShapeStub("driftline-coffee", fixtureRepoFiles())
	e := newGHTestEnv(t, stub.handle)
	e.tc.cfg.FoundationRoot = root
	e.tc.cfg.SandboxURL = "https://demo.holode.xyz"
	e.tc.cfg.RailsTemplate = "velaoc/foundation"
	spec := &AppSpec{
		Name:      "Driftline Coffee",
		Purpose:   "Guests preorder coffee for pickup",
		Entities:  []SpecEntity{{Name: "Order"}},
		Workflows: []SpecWorkflow{{Name: "preorder", Description: "Guest places a pickup order"}},
		Modules:   []string{"storefront"},
		SeedDemo:  "Three drinks",
	}
	out := createAndShape(e.g, "velaoc/foundation", "driftline-coffee", "coffee app", false, spec, e.tc.cfg)
	if !stub.generated {
		t.Fatal("expected template generate")
	}
	if strings.Contains(out, "shape error") {
		t.Fatalf("shape failed: %s", out)
	}
	if !strings.Contains(out, "Driftline Coffee") || !strings.Contains(out, "stamped identity") {
		t.Fatalf("summary missing identity: %s", out)
	}
	if !strings.Contains(out, "Do not re-run shape") {
		t.Fatalf("result should tell model not to redo shape: %s", out)
	}
	yml := stub.contentOf(foundationYMLPath)
	if !strings.Contains(yml, `application_name: "Driftline Coffee"`) {
		t.Fatalf("identity not stamped:\n%s", yml)
	}
	if strings.Contains(yml, "example.com") || strings.Contains(yml, `"Application"`) {
		t.Fatalf("placeholders survived:\n%s", yml)
	}
	readme := stub.contentOf(readmePath)
	if !strings.Contains(readme, identityStart) || !strings.Contains(readme, "Guests preorder") {
		t.Fatalf("app README missing:\n%s", readme)
	}
	if strings.Contains(readme, "starter template") || strings.Contains(readme, "A production-ready Rails application.") {
		t.Fatalf("foundation README placeholder text survived:\n%s", readme)
	}
	// storefront kept, crm omitted
	if stub.deleted("app/models/foundation/storefront/product.rb") {
		t.Fatal("selected storefront must stay")
	}
	if !stub.deleted("app/models/foundation/crm/contact.rb") {
		t.Fatal("unselected crm must be omitted")
	}
	if !strings.Contains(out, "crm") || !strings.Contains(out, "storefront") {
		t.Fatalf("summary should list omit/keep: %s", out)
	}
}

func TestCreateRailsAppStampsIdentityWithoutAppSpec(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	stub := newShapeStub("driftline-coffee", fixtureRepoFiles())
	e := newGHTestEnv(t, stub.handle)
	e.tc.cfg.FoundationRoot = root
	e.tc.cfg.SandboxURL = "https://demo.holode.xyz"
	out := createAndShape(e.g, "velaoc/foundation", "driftline-coffee", "desc", false, nil, e.tc.cfg)
	if strings.Contains(out, "shape error") {
		t.Fatalf("shape failed: %s", out)
	}
	if !strings.Contains(out, "Driftline Coffee") {
		t.Fatalf("identity should derive from app name: %s", out)
	}
	if !strings.Contains(out, "module omission skipped") {
		t.Fatalf("should report omit skip without app_spec: %s", out)
	}
	yml := stub.contentOf(foundationYMLPath)
	if !strings.Contains(yml, `application_name: "Driftline Coffee"`) {
		t.Fatalf("identity not stamped from name:\n%s", yml)
	}
	if strings.Contains(yml, "example.com") {
		t.Fatalf("example.com must not survive without app_spec:\n%s", yml)
	}
	if !strings.Contains(yml, "driftline-coffee.demo.holode.xyz") {
		t.Fatalf("domain not stamped:\n%s", yml)
	}
	readme := stub.contentOf(readmePath)
	if readme == "" || strings.Contains(readme, "starter template") {
		t.Fatalf("minimal app README required:\n%s", readme)
	}
	// Without app_spec, modules are kept (no deletes of module paths).
	if stub.deleted("app/models/foundation/crm/contact.rb") {
		t.Fatal("without app_spec, modules must not be omitted")
	}
}

func TestCreateRailsAppOmitFailureStillStampsIdentity(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	stub := newShapeStub("broken-app", fixtureRepoFiles())
	e := newGHTestEnv(t, stub.handle)
	e.tc.cfg.FoundationRoot = root
	e.tc.cfg.SandboxURL = "https://demo.holode.xyz"
	// Unknown module makes full planShape fail; identity must still land.
	spec := &AppSpec{
		Name:      "Broken App",
		Purpose:   "Should still get a real domain",
		Entities:  []SpecEntity{{Name: "Thing"}},
		Workflows: []SpecWorkflow{{Name: "do"}},
		Modules:   []string{"storefront", "ghost_module"},
	}
	out := createAndShape(e.g, "velaoc/foundation", "broken-app", "", false, spec, e.tc.cfg)
	if strings.Contains(out, "shape error") && !strings.Contains(out, "stamped identity") {
		t.Fatalf("omit failure must not abort identity: %s", out)
	}
	if !strings.Contains(out, "module omission skipped") {
		t.Fatalf("must report omit skip: %s", out)
	}
	if !strings.Contains(out, "Broken App") {
		t.Fatalf("identity name missing from summary: %s", out)
	}
	yml := stub.contentOf(foundationYMLPath)
	if !strings.Contains(yml, `application_name: "Broken App"`) {
		t.Fatalf("identity not stamped after omit failure:\n%s", yml)
	}
	if strings.Contains(yml, "example.com") {
		t.Fatalf("example.com survived omit failure:\n%s", yml)
	}
	if !strings.Contains(yml, "broken-app.demo.holode.xyz") {
		t.Fatalf("domain missing after omit failure:\n%s", yml)
	}
	// Modules should not have been deleted when omit was skipped.
	if stub.deleted("app/models/foundation/crm/contact.rb") {
		t.Fatal("failed omit must not partially delete modules")
	}
}

func TestIdentityFromSpecDerivesNameWithoutSpec(t *testing.T) {
	id := identityFromSpec(nil, "my-cool-shop", "https://demo.holode.xyz")
	if id.ApplicationName != "My Cool Shop" {
		t.Fatalf("name=%q", id.ApplicationName)
	}
	if id.Domain != "my-cool-shop.demo.holode.xyz" {
		t.Fatalf("domain=%q", id.Domain)
	}
	if id.Domain == "example.com" || id.ApplicationName == "Application" {
		t.Fatal("placeholders must not be used")
	}
}

func TestPlanShapeNilSpecKeepsAllModules(t *testing.T) {
	plan, err := planShape(fixtureRepoFiles(), nil, identityFromSpec(nil, "x", ""), fixtureModules())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Omitted) != 0 || len(plan.Kept) != 2 {
		t.Fatalf("kept=%v omitted=%v", plan.Kept, plan.Omitted)
	}
	if !strings.Contains(plan.Writes[foundationYMLPath], `application_name: "X"`) {
		t.Fatalf("identity missing:\n%s", plan.Writes[foundationYMLPath])
	}
}

func TestShapeRemoteIdentityAndOmit(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	stub := newShapeStub("driftline-coffee", fixtureRepoFiles())
	e := newGHTestEnv(t, stub.handle)
	e.tc.cfg.FoundationRoot = root
	e.tc.cfg.SandboxURL = "https://demo.holode.xyz"
	spec := &AppSpec{
		Name:      "Driftline Coffee",
		Purpose:   "Guests preorder coffee for pickup",
		Entities:  []SpecEntity{{Name: "Order"}},
		Workflows: []SpecWorkflow{{Name: "preorder"}},
		Modules:   []string{"storefront"},
		SeedDemo:  "Three drinks",
	}
	out := e.g.shapeGeneratedApp("driftline-coffee", spec, e.tc.cfg)
	if strings.HasPrefix(out, "shape error") {
		t.Fatalf("shape failed: %s", out)
	}
	if !strings.Contains(out, "Driftline Coffee") || !strings.Contains(out, "crm") {
		t.Fatalf("summary: %s", out)
	}
	if !stub.treePosted || !stub.commitPosted || !stub.refPatched {
		t.Fatalf("git data API not used: tree=%v commit=%v ref=%v", stub.treePosted, stub.commitPosted, stub.refPatched)
	}
	yml := stub.contentOf(foundationYMLPath)
	if !strings.Contains(yml, `application_name: "Driftline Coffee"`) || strings.Contains(yml, "example.com") {
		t.Fatalf("yml bad: %s", yml)
	}
	readme := stub.contentOf(readmePath)
	if !strings.Contains(readme, identityStart) || !strings.Contains(readme, "Guests preorder") {
		t.Fatalf("readme bad: %s", readme)
	}
	if !stub.deleted("app/models/foundation/crm/contact.rb") || !stub.deleted("config/foundation/modules/crm.yml") {
		t.Fatal("expected crm path deletes in tree commit")
	}
}

func TestStandaloneShapeActionDispatch(t *testing.T) {
	cfg := testCfg(t)
	cfg.GitHubToken = "t"
	tc := &ToolCtx{cfg: cfg, authorID: "dev"}
	out := tc.runGithub(toolArgs{Action: "shape"})
	if !strings.Contains(out, "shape needs repo") {
		t.Fatalf("shape should require repo: %s", out)
	}
}

func TestExistingGithubActionsUnaffected(t *testing.T) {
	// put_file / search_code still work without app_spec.
	blobs := fixtureBlobs()
	e := newGHTestEnv(t, func(w http.ResponseWriter, r *http.Request, e *ghTestEnv) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, 200, map[string]any{"login": "velaoc"})
		case r.URL.Path == "/repos/velaoc/demo":
			writeJSON(w, 200, map[string]any{"default_branch": "main"})
		case strings.HasPrefix(r.URL.Path, "/repos/velaoc/demo/git/trees/"):
			writeJSON(w, 200, fixtureTree())
		case strings.HasPrefix(r.URL.Path, "/repos/velaoc/demo/git/blobs/"):
			sha := filepath.Base(r.URL.Path)
			content := blobs[sha]
			writeJSON(w, 200, map[string]any{
				"content": base64.StdEncoding.EncodeToString([]byte(content)),
			})
		default:
			writeJSON(w, 404, map[string]any{"message": r.URL.Path})
		}
	})
	out := e.g.searchCode("velaoc/demo", "create", "", "", "main")
	if !strings.Contains(out, "user.rb") {
		t.Fatalf("search_code broken: %s", out)
	}
}
