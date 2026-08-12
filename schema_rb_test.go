package main

import (
	"os"
	"strings"
	"testing"
)

func loadSchemaFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/schema_rb_fixture.rb")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSchemaRBFoundationShape(t *testing.T) {
	s, err := parseSchemaRB(loadSchemaFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "2026_08_11_140000" {
		t.Fatalf("version=%q", s.Version)
	}
	wantTables := []string{
		"active_storage_attachments", "crm_companies", "crm_contacts",
		"storefront_orders", "storefront_line_items", "users",
	}
	if len(s.Order) != len(wantTables) {
		t.Fatalf("order=%v", s.Order)
	}
	for _, name := range wantTables {
		if _, ok := s.Tables[name]; !ok {
			t.Fatalf("missing table %s", name)
		}
	}

	// columns + types from real create_table / t.<type> lines
	u := s.Tables["users"]
	col := map[string]schemaCol{}
	for _, c := range u.Columns {
		col[c.Name] = c
	}
	if c, ok := col["id"]; !ok || c.Type != "bigint" || !strings.Contains(c.Extra, "pk") {
		t.Fatalf("implicit id pk: %+v", col["id"])
	}
	if c := col["email"]; c.Type != "string" || !c.NotNull || c.Default != `""` {
		t.Fatalf("email col: %+v", c)
	}
	if c := col["admin"]; c.Type != "boolean" || !c.NotNull {
		t.Fatalf("admin col: %+v", c)
	}
	// indexes
	if len(u.Indexes) < 4 {
		t.Fatalf("users indexes: %+v", u.Indexes)
	}
	foundUniqueEmail := false
	for _, idx := range u.Indexes {
		if idx.Unique && len(idx.Columns) == 1 && idx.Columns[0] == "email" {
			foundUniqueEmail = true
		}
	}
	if !foundUniqueEmail {
		t.Fatalf("missing unique email index: %+v", u.Indexes)
	}

	// t.index, t.check_constraint inside create_table
	co := s.Tables["crm_companies"]
	if len(co.Checks) == 0 {
		t.Fatal("crm_companies missing check_constraint")
	}
	if !strings.Contains(co.Checks[0].Name, "name_present") {
		t.Fatalf("check name: %+v", co.Checks[0])
	}
	// unique partial index with where:
	foundPartial := false
	for _, idx := range co.Indexes {
		if idx.Unique && idx.Where != "" && len(idx.Columns) == 2 {
			foundPartial = true
		}
	}
	if !foundPartial {
		t.Fatalf("expected partial unique index: %+v", co.Indexes)
	}

	// add_foreign_key
	li := s.Tables["storefront_line_items"]
	if len(li.FKs) < 2 {
		t.Fatalf("line_items fks: %+v", li.FKs)
	}
	foundOrderFK := false
	for _, fk := range li.FKs {
		if fk.ToTable == "storefront_orders" && fk.Column == "order_id" && fk.OnDelete == "cascade" {
			foundOrderFK = true
		}
	}
	if !foundOrderFK {
		t.Fatalf("order_id fk: %+v", li.FKs)
	}

	// storefront_orders: many checks
	so := s.Tables["storefront_orders"]
	if len(so.Checks) < 4 {
		t.Fatalf("orders checks: %+v", so.Checks)
	}
	if c := mapCols(so)["currency"]; c.Limit != "3" {
		t.Fatalf("currency limit: %+v", c)
	}
}

func mapCols(t *schemaTable) map[string]schemaCol {
	m := map[string]schemaCol{}
	for _, c := range t.Columns {
		m[c.Name] = c
	}
	return m
}

func TestFormatSchemaInspectOneTable(t *testing.T) {
	s, err := parseSchemaRB(loadSchemaFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	out := formatSchemaInspect(s, []string{"users"})
	if !strings.Contains(out, "users\n") {
		t.Fatalf("header: %s", out)
	}
	if !strings.Contains(out, "email:string") || !strings.Contains(out, "not_null") {
		t.Fatalf("columns: %s", out)
	}
	if !strings.Contains(out, "unique(email)") {
		t.Fatalf("index: %s", out)
	}
	// compact: a few lines, not a dump
	lines := strings.Count(out, "\n") + 1
	if lines > 8 {
		t.Fatalf("too many lines for one table (%d):\n%s", lines, out)
	}
	if len(out) > 1500 {
		t.Fatalf("one table too large: %d bytes", len(out))
	}
}

func TestFormatSchemaInspectSeveralCompact(t *testing.T) {
	s, err := parseSchemaRB(loadSchemaFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	out := formatSchemaInspect(s, []string{"users", "crm_companies", "storefront_orders"})
	for _, name := range []string{"users", "crm_companies", "storefront_orders"} {
		if !strings.Contains(out, name+"\n") {
			t.Fatalf("missing %s in:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "organization_id:bigint") {
		t.Fatalf("crm cols: %s", out)
	}
	if !strings.Contains(out, "fk:") || !strings.Contains(out, "check:") {
		t.Fatalf("missing fk/check lines: %s", out)
	}
	// worst-case-ish multi-table still far under full schema
	if len(out) > 4000 {
		t.Fatalf("several tables not compact enough: %d bytes\n%s", len(out), out)
	}
}

func TestFormatSchemaInspectListNames(t *testing.T) {
	s, err := parseSchemaRB(loadSchemaFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	out := formatSchemaInspect(s, nil)
	if !strings.Contains(out, "tables (6):") {
		t.Fatalf("list header: %s", out)
	}
	for _, name := range []string{
		"active_storage_attachments", "crm_companies", "crm_contacts",
		"storefront_orders", "storefront_line_items", "users",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("missing name %s: %s", name, out)
		}
	}
	// listing must stay cheap
	if len(out) > 500 {
		t.Fatalf("list too large: %d", len(out))
	}
	if strings.Contains(out, "cols:") {
		t.Fatalf("list should not describe columns: %s", out)
	}
}

func TestFormatSchemaInspectUnknownSuggests(t *testing.T) {
	s, err := parseSchemaRB(loadSchemaFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	out := formatSchemaInspect(s, []string{"user", "crm_companie"})
	if !strings.Contains(out, `unknown table "user"`) {
		t.Fatalf("unknown user: %s", out)
	}
	if !strings.Contains(out, "closest:") || !strings.Contains(out, "users") {
		t.Fatalf("suggest users: %s", out)
	}
	if !strings.Contains(out, "crm_companies") {
		t.Fatalf("suggest crm_companies: %s", out)
	}
}

func TestParseSchemaRBMissingAndUnparseable(t *testing.T) {
	if _, err := parseSchemaRB(""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty: %v", err)
	}
	if _, err := parseSchemaRB("# just a comment\n"); err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("comment only: %v", err)
	}
	if _, err := parseSchemaRB("ActiveRecord::Schema[8.1].define(version: 1) do\nend\n"); err == nil ||
		!strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("no tables: %v", err)
	}
	// format never returns confident empty success without tables — parse fails first
	out := formatSchemaInspect(&schemaRB{Version: "1", Tables: map[string]*schemaTable{}, Order: nil}, nil)
	if !strings.Contains(out, "tables (0)") {
		// empty schema object is internal only; public path errors before this
		t.Fatalf("empty object list: %s", out)
	}
}

func TestClosestTableNames(t *testing.T) {
	names := []string{"users", "crm_users", "sessions", "storefront_orders"}
	got := closestTableNames("user", names, 3)
	if len(got) == 0 || got[0] != "users" {
		t.Fatalf("got %v", got)
	}
}
