package main

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
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
	"db/schema.rb":      "add a migration under db/migrate/ instead — schema.rb is regenerated from migrations and any edit here is lost",
	"db/structure.sql":  "add a migration under db/migrate/ instead — structure.sql is regenerated from migrations and any edit here is lost",
	"Gemfile.lock":      "change the Gemfile instead — Gemfile.lock is produced by bundler",
	"yarn.lock":         "change package.json instead — yarn.lock is produced by the package manager",
	"package-lock.json": "change package.json instead — package-lock.json is produced by the package manager",
}

// refuseGeneratedArtifact returns an explanation when a path must not be
// written directly, and "" when the write is fine.
func refuseGeneratedArtifact(p string) string {
	clean := cleanRepoPath(p)
	advice, ok := generatedArtifacts[clean]
	if !ok {
		return ""
	}
	return clean + " is generated and must not be edited directly: " + advice
}

func cleanRepoPath(p string) string {
	clean := path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "./"))
	return strings.TrimPrefix(clean, "/")
}

// --- Migration timestamp guard ---
//
// A migration whose version is ≤ the schema.rb stamp is marked applied by
// db:schema:load and silently never runs. Refuse the write and name the
// minimum acceptable timestamp so the next attempt is correct (never silently
// rename — that would hide the same class of bug).

var (
	schemaVersionRE   = regexp.MustCompile(`define\s*\(\s*version:\s*([0-9_]+)`)
	migrationFileRE   = regexp.MustCompile(`^(\d{14})_.*\.rb$`)
	schemaMutationRE  = regexp.MustCompile(`\b(add_column|remove_column|add_index|remove_index|add_check_constraint|add_reference|change_column|create_table|drop_table)\s*[\(:"']`)
)

// schemaVersionFromContent parses ActiveRecord::Schema.define(version: …).
// Accepts underscored (2026_08_11_140000) and plain (20260811140000) forms.
// ok is false when no stamp is present.
func schemaVersionFromContent(schemaRB string) (int64, bool) {
	m := schemaVersionRE.FindStringSubmatch(schemaRB)
	if m == nil {
		return 0, false
	}
	digits := strings.ReplaceAll(m[1], "_", "")
	if digits == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// migrationVersionFromPath extracts the 14-digit version from a db/migrate/
// filename. ok is false when the path is not a versioned migration.
func migrationVersionFromPath(p string) (int64, bool) {
	clean := cleanRepoPath(p)
	if !strings.HasPrefix(clean, "db/migrate/") {
		return 0, false
	}
	return migrationVersionFromName(path.Base(clean))
}

func migrationVersionFromName(base string) (int64, bool) {
	m := migrationFileRE.FindStringSubmatch(base)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// maxMigrationVersion returns the highest version among migration basenames,
// skipping skipBase when non-empty (the file being written, so self-updates
// are not compared against themselves).
func maxMigrationVersion(names []string, skipBase string) int64 {
	var max int64
	for _, name := range names {
		base := path.Base(name)
		if skipBase != "" && base == skipBase {
			continue
		}
		if v, ok := migrationVersionFromName(base); ok && v > max {
			max = v
		}
	}
	return max
}

// refuseStaleMigration refuses a migration version that would never run.
// floor is max(schemaStamp, maxOtherMigrations); the proposed version must
// be strictly greater. Message always states the minimum acceptable value.
func refuseStaleMigration(proposed, schemaStamp, maxOther int64) string {
	floor := schemaStamp
	if maxOther > floor {
		floor = maxOther
	}
	if proposed > floor {
		return ""
	}
	min := floor + 1
	return fmt.Sprintf(
		"migration timestamp %d is not later than the schema stamp/existing migrations (floor %d); use a timestamp of at least %d — refused (not renamed)",
		proposed, floor, min,
	)
}

// isMigrationDir reports whether p is under db/migrate/ (schema mutations allowed).
func isMigrationDir(p string) bool {
	clean := cleanRepoPath(p)
	return clean == "db/migrate" || strings.HasPrefix(clean, "db/migrate/")
}

// --- Runtime schema-mutation guard ---
//
// Mutating the schema from seeds/lib/app races parallel test workers and
// diverges from db:schema:load. Real Ruby calls to DDL helpers outside
// db/migrate/ are refused; comments, strings, and bare mentions are not.

// refuseRuntimeSchemaMutation returns an explanation when content performs
// schema DDL outside db/migrate/, and "" when the write is fine.
func refuseRuntimeSchemaMutation(p, content string) string {
	if isMigrationDir(p) {
		return ""
	}
	if !hasSchemaMutationCall(content) {
		return ""
	}
	clean := cleanRepoPath(p)
	if clean == "" {
		clean = p
	}
	return clean + " must not mutate the schema at runtime: put this in a migration under db/migrate/ instead"
}

// hasSchemaMutationCall reports a real Ruby call to a DDL helper.
// Comments and string/regex literals are stripped first so mentions and
// assertions (e.g. assert_includes body, "add_column") do not false-positive.
func hasSchemaMutationCall(content string) bool {
	return schemaMutationRE.MatchString(stripRubyCommentsAndStrings(content))
}

// stripRubyCommentsAndStrings removes # line comments and simple '…' / "…"
// string literals so residual text only contains real code tokens. Not a full
// Ruby parser — enough to avoid the documented false-positive classes.
func stripRubyCommentsAndStrings(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	i := 0
	for i < len(src) {
		c := src[i]
		switch c {
		case '#':
			// line comment
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case '\'', '"':
			quote := c
			i++
			for i < len(src) {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				if src[i] == quote {
					i++
					break
				}
				if src[i] == '\n' {
					b.WriteByte('\n')
				}
				i++
			}
			b.WriteByte(' ')
		case '/':
			// regex literal only when it looks like one (prev non-space is
			// not a value token). Conservative: treat /…/ as noise when the
			// previous written byte is not a word/digit/')'/']'.
			prev := byte(' ')
			if b.Len() > 0 {
				prev = b.String()[b.Len()-1]
			}
			if isRegexContext(prev) {
				i++
				for i < len(src) {
					if src[i] == '\\' && i+1 < len(src) {
						i += 2
						continue
					}
					if src[i] == '/' {
						i++
						// skip trailing flags
						for i < len(src) && ((src[i] >= 'a' && src[i] <= 'z') || (src[i] >= 'A' && src[i] <= 'Z')) {
							i++
						}
						break
					}
					if src[i] == '\n' {
						break
					}
					i++
				}
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isRegexContext(prev byte) bool {
	// After these, `/` is division or end of something, not a regex start.
	switch prev {
	case ' ', '\t', '\n', '(', ',', '=', '!', '~', '&', '|', '?', ':', '[', '{', ';':
		return true
	}
	return false
}
