package main

import "testing"

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
