package main

import (
	"path"
	"strings"
)

// Files Rails generates from something else. Hand-editing them is always a
// mistake: the edit is silently discarded the next time the generator runs,
// and a bad edit corrupts the artifact.
//
// This is enforced rather than advised. A benchmark build patched db/schema.rb
// thirty-three times, corrupted the storefront_orders block into invalid Ruby
// ("expecting end-of-input"), failed verification on it, and then spent turns
// repairing damage it had caused — while a hand-edited schema also diverges
// from what db:migrate produces, so the repository and a fresh deploy disagree.
var generatedArtifacts = map[string]string{
	"db/schema.rb":     "add a migration under db/migrate/ instead — schema.rb is regenerated from migrations and any edit here is lost",
	"db/structure.sql": "add a migration under db/migrate/ instead — structure.sql is regenerated from migrations and any edit here is lost",
	"Gemfile.lock":     "change the Gemfile instead — Gemfile.lock is produced by bundler",
	"yarn.lock":        "change package.json instead — yarn.lock is produced by the package manager",
	"package-lock.json": "change package.json instead — package-lock.json is produced by the package manager",
}

// refuseGeneratedArtifact returns an explanation when a path must not be
// written directly, and "" when the write is fine.
func refuseGeneratedArtifact(p string) string {
	clean := path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "./"))
	clean = strings.TrimPrefix(clean, "/")
	advice, ok := generatedArtifacts[clean]
	if !ok {
		return ""
	}
	return clean + " is generated and must not be edited directly: " + advice
}
