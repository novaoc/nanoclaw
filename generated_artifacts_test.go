package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefuseGeneratedArtifact(t *testing.T) {
	for _, tc := range []struct {
		path    string
		refused bool
	}{
		{"db/schema.rb", true},
		{"./db/schema.rb", true},
		{"/db/schema.rb", true},
		{"db/structure.sql", true},
		{"Gemfile.lock", true},
		{"db/migrate/20260811080000_add_shipping.rb", false},
		{"app/models/order.rb", false},
		{"config/routes.rb", false},
		{"Gemfile", false},
		{"docs/db/schema.rb", false}, // only the real one, not any lookalike
	} {
		got := refuseGeneratedArtifact(tc.path)
		if tc.refused && got == "" {
			t.Errorf("%s should be refused: hand-edits corrupt it and are discarded on regeneration", tc.path)
		}
		if !tc.refused && got != "" {
			t.Errorf("%s should be writable, got refusal: %s", tc.path, got)
		}
	}
}

func TestRefusalNamesTheAlternative(t *testing.T) {
	why := refuseGeneratedArtifact("db/schema.rb")
	if why == "" {
		t.Fatal("expected a refusal")
	}
	// A refusal that does not say what to do instead just stalls the build.
	if !contains(why, "migration") {
		t.Errorf("refusal must point at migrations, got: %s", why)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (stringIndex(s, sub) >= 0) }
func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSchemaVersionFromContent(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    int64
		wantOK  bool
	}{
		{
			name:   "underscored form",
			body:   "ActiveRecord::Schema[8.1].define(version: 2026_08_11_140000) do\nend\n",
			want:   20260811140000,
			wantOK: true,
		},
		{
			name:   "plain digits",
			body:   "ActiveRecord::Schema[7.2].define(version: 20260811140000) do\n",
			want:   20260811140000,
			wantOK: true,
		},
		{
			name:   "spaces around version",
			body:   "define( version: 2026_01_02_030405 )",
			want:   20260102030405,
			wantOK: true,
		},
		{
			name:   "absent",
			body:   "# no stamp here\nActiveRecord::Schema.define do\nend\n",
			wantOK: false,
		},
		{
			name:   "empty file",
			body:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := schemaVersionFromContent(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (got %d)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("version=%d want %d", got, tc.want)
			}
		})
	}
}

func TestRefuseStaleMigration(t *testing.T) {
	// Stamp 20260811140000, existing max 20260811135000 → floor 140000
	const stamp = int64(20260811140000)
	const existing = int64(20260811135000)

	// at stamp → refuse, message names minimum
	why := refuseStaleMigration(20260811140000, stamp, existing)
	if why == "" {
		t.Fatal("equal to stamp must be refused")
	}
	if !contains(why, "20260811140001") {
		t.Errorf("refusal must state minimum acceptable timestamp, got: %s", why)
	}

	// below stamp → refuse
	why = refuseStaleMigration(20260811130000, stamp, existing)
	if why == "" || !contains(why, "20260811140001") {
		t.Errorf("below stamp must refuse with min, got: %s", why)
	}

	// above stamp and existing → accept
	if why := refuseStaleMigration(20260811140001, stamp, existing); why != "" {
		t.Errorf("later timestamp must be accepted, got: %s", why)
	}
	if why := refuseStaleMigration(20260811150000, stamp, existing); why != "" {
		t.Errorf("well later timestamp must be accepted, got: %s", why)
	}

	// existing higher than stamp sets the floor
	why = refuseStaleMigration(20260811150000, stamp, 20260811160000)
	if why == "" || !contains(why, "20260811160001") {
		t.Errorf("must clear max existing, got: %s", why)
	}

	// no stamp, no existing → any positive version ok
	if why := refuseStaleMigration(1, 0, 0); why != "" {
		t.Errorf("no floor should accept, got: %s", why)
	}
}

func TestMigrationVersionFromPath(t *testing.T) {
	v, ok := migrationVersionFromPath("db/migrate/20260811130000_add_coffee_fields_to_storefront_products.rb")
	if !ok || v != 20260811130000 {
		t.Fatalf("got %d ok=%v", v, ok)
	}
	if _, ok := migrationVersionFromPath("app/models/x.rb"); ok {
		t.Fatal("non-migration path")
	}
	if _, ok := migrationVersionFromPath("db/migrate/not_a_timestamp.rb"); ok {
		t.Fatal("unversioned migrate file")
	}
}

func TestRefuseRuntimeSchemaMutation(t *testing.T) {
	call := "connection.add_column :storefront_products, :roast_level, :string\n"
	for _, p := range []string{
		"lib/foundation/demo_seeds.rb",
		"app/models/product.rb",
		"db/seeds.rb",
	} {
		why := refuseRuntimeSchemaMutation(p, call)
		if why == "" {
			t.Errorf("%s with add_column must be refused", p)
		}
		if !contains(why, "migration") {
			t.Errorf("%s refusal must point at migrations: %s", p, why)
		}
	}

	// allowed inside db/migrate/
	if why := refuseRuntimeSchemaMutation("db/migrate/20260811150000_add_roast.rb", call); why != "" {
		t.Errorf("migrate path must allow DDL, got: %s", why)
	}

	// comment is not a call
	if why := refuseRuntimeSchemaMutation("lib/x.rb", "# connection.add_column :t, :c, :string\n"); why != "" {
		t.Errorf("comment must not refuse: %s", why)
	}

	// string mention is not a call
	if why := refuseRuntimeSchemaMutation("test/x_test.rb", "assert_includes body, \"add_column\"\n"); why != "" {
		t.Errorf("string must not refuse: %s", why)
	}
	if why := refuseRuntimeSchemaMutation("test/x_test.rb", "expect(sql).to include('add_column')\n"); why != "" {
		t.Errorf("single-quoted string must not refuse: %s", why)
	}

	// bare name without call syntax
	if why := refuseRuntimeSchemaMutation("lib/x.rb", "methods = %w[add_column remove_column]\n"); why != "" {
		t.Errorf("bare mention must not refuse: %s", why)
	}

	// each DDL helper is caught
	for _, fn := range []string{
		"add_column", "remove_column", "add_index", "remove_index",
		"add_check_constraint", "add_reference", "change_column",
		"create_table", "drop_table",
	} {
		body := fn + " :things, :x\n"
		if why := refuseRuntimeSchemaMutation("lib/hack.rb", body); why == "" {
			t.Errorf("%s call must be refused", fn)
		}
	}

	// paren form
	if why := refuseRuntimeSchemaMutation("app/x.rb", "create_table(:users) do |t|\nend\n"); why == "" {
		t.Error("create_table( must be refused outside migrate")
	}
}

func TestWritePathsMigrationTimestampGuard(t *testing.T) {
	coder, ws := testCoder(t)
	// Real schema stamp + one existing migration.
	if err := os.MkdirAll(filepath.Join(ws, "db", "migrate"), 0o755); err != nil {
		t.Fatal(err)
	}
	schema := "ActiveRecord::Schema[8.1].define(version: 2026_08_11_140000) do\nend\n"
	if err := os.WriteFile(filepath.Join(ws, "db", "schema.rb"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "db", "migrate", "20260811120000_init.rb"), []byte("class Init < ActiveRecord::Migration[8.1]; end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// at/below stamp refused with minimum
	out := coder.writeWorkspaceFile(
		"db/migrate/20260811130000_add_coffee_fields_to_storefront_products.rb",
		"class AddCoffee < ActiveRecord::Migration[8.1]\n  def change\n    add_column :storefront_products, :roast_level, :string\n  end\nend\n",
	)
	if !strings.Contains(out, "write error:") || !strings.Contains(out, "20260811140001") {
		t.Fatalf("stale migration must refuse with min timestamp, got: %s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "db", "migrate", "20260811130000_add_coffee_fields_to_storefront_products.rb")); err == nil {
		t.Fatal("refused migration must not be written to disk")
	}

	// equal to stamp refused
	out = coder.writeWorkspaceFile(
		"db/migrate/20260811140000_noop.rb",
		"class Noop < ActiveRecord::Migration[8.1]; def change; end; end\n",
	)
	if !strings.Contains(out, "20260811140001") {
		t.Fatalf("equal stamp must refuse with min, got: %s", out)
	}

	// above both accepted; add_column inside migrate is fine
	out = coder.writeWorkspaceFile(
		"db/migrate/20260811150000_add_roast_level.rb",
		"class AddRoastLevel < ActiveRecord::Migration[8.1]\n  def change\n    add_column :storefront_products, :roast_level, :string\n  end\nend\n",
	)
	if !strings.Contains(out, "wrote ") {
		t.Fatalf("valid migration must be accepted, got: %s", out)
	}
}

func TestWritePathsRefuseRuntimeSchemaMutation(t *testing.T) {
	coder, ws := testCoder(t)
	call := "ActiveRecord::Base.connection.add_column :storefront_products, :roast_level, :string\n"

	for _, p := range []string{"lib/foundation/demo_seeds.rb", "app/models/x.rb", "db/seeds.rb"} {
		out := coder.writeWorkspaceFile(p, call)
		if !strings.Contains(out, "write error:") || !strings.Contains(out, "migration") {
			t.Fatalf("%s must refuse runtime DDL, got: %s", p, out)
		}
		if _, err := os.Stat(filepath.Join(ws, p)); err == nil {
			t.Fatalf("%s must not be written", p)
		}
	}

	// comment / string still writable
	out := coder.writeWorkspaceFile("lib/ok.rb", "# add_column is only for migrations\ns = \"add_column\"\n")
	if !strings.Contains(out, "wrote ") {
		t.Fatalf("comment/string file must write, got: %s", out)
	}

	// ordinary non-DDL write still works
	out = coder.writeWorkspaceFile("app/models/product.rb", "class Product < ApplicationRecord\nend\n")
	if !strings.Contains(out, "wrote ") {
		t.Fatalf("ordinary write must work, got: %s", out)
	}
}

func TestApplyPatchRefusesRuntimeSchemaMutation(t *testing.T) {
	coder, ws := testCoder(t)
	path := "lib/foundation/demo_seeds.rb"
	full := filepath.Join(ws, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := "module DemoSeeds\n  def self.run\n    puts :ok\n  end\nend\n"
	if err := os.WriteFile(full, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	out := coder.applyPatch(path, []patchOp{{
		Op:      "replace",
		Find:    "puts :ok",
		Replace: "connection.add_column :storefront_products, :roast_level, :string",
	}})
	if !strings.Contains(out, "patch error:") || !strings.Contains(out, "migration") {
		t.Fatalf("patch introducing DDL must refuse, got: %s", out)
	}
	got, _ := os.ReadFile(full)
	if string(got) != initial {
		t.Fatal("refused patch must leave file unchanged")
	}
}
