package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// shape_remote.go — apply ShapePlan to a GitHub repo after create_rails_app.
// Uses the Git Data API so identity, README, and module omission land in one
// commit (contents API would be one commit per file).

// shapeGeneratedApp stamps identity, rewrites the app README, and omits
// foundation modules the app_spec did not select. Identity always runs even
// when app_spec is nil or module omission cannot be done safely — placeholders
// like application_name "Application" / domain "example.com" must never ship.
// Returns a human-readable summary or an error string.
func (g *ghClient) shapeGeneratedApp(repo string, spec *AppSpec, cfg *Config) string {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "shape error: " + err.Error()
	}

	// Template generation is async — wait until foundation.yml is readable.
	if err := g.waitForFile(owner, name, foundationYMLPath, 45*time.Second); err != nil {
		return "shape error: generated app not ready — " + err.Error()
	}

	ref, err := g.defaultRef(owner, name, "")
	if err != nil {
		return "shape error: " + err.Error()
	}

	id := identityFromSpec(spec, name, cfg.SandboxURL)

	var declared map[string]*ModuleManifest
	var omitSkip string
	if strings.TrimSpace(cfg.FoundationRoot) == "" {
		omitSkip = "VELA_FOUNDATION_ROOT is not configured"
		declared = map[string]*ModuleManifest{}
	} else {
		declared, err = loadFullModuleManifests(cfg.FoundationRoot)
		if err != nil {
			omitSkip = err.Error()
			declared = map[string]*ModuleManifest{}
		}
	}

	// Build a path→content map of text files we may touch. Owned module paths
	// and scan roots are enough; we do not need binaries.
	files, err := g.fetchShapeFiles(owner, name, ref, declared)
	if err != nil {
		return "shape error: " + err.Error()
	}

	plan, notes := buildShapePlan(files, spec, id, declared, omitSkip)

	if len(plan.Writes) == 0 && len(plan.Deletes) == 0 {
		return formatShapeResult(owner, name, id, plan, notes, 0, 0)
	}

	msg := fmt.Sprintf("shape: identity %s; omit %s", id.ApplicationName, strings.Join(plan.Omitted, ", "))
	if len(plan.Omitted) == 0 {
		msg = fmt.Sprintf("shape: identity %s (all declared modules kept)", id.ApplicationName)
	}
	if err := g.commitShapePlan(owner, name, ref, msg, plan); err != nil {
		return "shape error: " + err.Error()
	}
	if g.cache != nil {
		g.cache.invalidateRepo(owner, name)
	}
	return formatShapeResult(owner, name, id, plan, notes, len(plan.Writes), len(plan.Deletes))
}

// buildShapePlan prefers full identity+omit+README. On omit failure (or when
// omission was already ruled out), falls back to identity+README only.
func buildShapePlan(files map[string]string, spec *AppSpec, id AppIdentity, declared map[string]*ModuleManifest, omitSkip string) (*ShapePlan, []string) {
	var notes []string
	if omitSkip != "" {
		notes = append(notes, "module omission skipped: "+omitSkip)
		plan, err := planIdentityOnly(files, spec, id, declared)
		if err != nil {
			// Should not happen if foundation.yml is present; surface as empty plan note.
			notes = append(notes, "identity stamp failed: "+err.Error())
			return &ShapePlan{Writes: map[string]string{}}, notes
		}
		return plan, notes
	}
	if spec == nil {
		notes = append(notes, "module omission skipped: no app_spec (all modules kept)")
		plan, err := planIdentityOnly(files, spec, id, declared)
		if err != nil {
			notes = append(notes, "identity stamp failed: "+err.Error())
			return &ShapePlan{Writes: map[string]string{}}, notes
		}
		return plan, notes
	}

	plan, err := planShape(files, spec, id, declared)
	if err != nil {
		notes = append(notes, "module omission skipped: "+err.Error())
		fallback, ferr := planIdentityOnly(files, spec, id, declared)
		if ferr != nil {
			notes = append(notes, "identity stamp failed: "+ferr.Error())
			return &ShapePlan{Writes: map[string]string{}}, notes
		}
		return fallback, notes
	}
	return plan, notes
}

func formatShapeResult(owner, name string, id AppIdentity, plan *ShapePlan, notes []string, nWrite, nDel int) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"shaped %s/%s: stamped identity %q domain=%s support=%s; omitted modules [%s]; kept [%s]; %d files written, %d deleted. Do not re-run shape — identity, README, and module selection are already applied.",
		owner, name, id.ApplicationName, id.Domain, id.SupportEmail,
		strings.Join(plan.Omitted, ", "), strings.Join(plan.Kept, ", "),
		nWrite, nDel,
	)
	for _, n := range notes {
		b.WriteString("\n")
		b.WriteString(n)
	}
	return b.String()
}

func (g *ghClient) waitForFile(owner, name, path string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		_, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s", owner, name, ghEsc(path)), nil)
		if err == nil && st == 200 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("HTTP %d", st)
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("%s not available after wait (%s)", path, last)
}

// fetchShapeFiles loads repo blobs needed for shaping: foundation.yml, README,
// schema, every module-owned path present, every module manifest, and text
// files under omit scan roots (for marker stripping / residue).
func (g *ghClient) fetchShapeFiles(owner, name, ref string, declared map[string]*ModuleManifest) (map[string]string, error) {
	entries, err := g.cachedTree(owner, name, ref)
	if err != nil {
		return nil, err
	}

	need := map[string]bool{
		foundationYMLPath: true,
		readmePath:        true,
		"db/schema.rb":    true,
	}
	for _, m := range declared {
		need[moduleManifestDir+"/"+m.Name+".yml"] = true
	}

	// All text-ish blobs under scan roots, plus anything under owned paths.
	var shas []ghTreeBlob
	for _, e := range entries {
		p := filepath.ToSlash(e.Path)
		if searchSkipRemotePath(p) {
			continue
		}
		if e.Size > maxSearchFileBytes {
			continue
		}
		if need[p] || underOmitScanRoot(p) || ownedByAny(declared, p) {
			shas = append(shas, e)
		}
	}

	out := map[string]string{}
	for _, e := range shas {
		raw, err := g.cachedBlob(owner, name, e.SHA)
		if err != nil {
			continue
		}
		// Skip obvious binaries.
		if strings.IndexByte(string(raw), 0) >= 0 {
			continue
		}
		out[filepath.ToSlash(e.Path)] = string(raw)
	}
	if _, ok := out[foundationYMLPath]; !ok {
		return nil, fmt.Errorf("%s missing from repository tree", foundationYMLPath)
	}
	if _, ok := out[readmePath]; !ok {
		return nil, fmt.Errorf("%s missing from repository tree", readmePath)
	}
	return out, nil
}

func ownedByAny(declared map[string]*ModuleManifest, path string) bool {
	for _, m := range declared {
		if ownedPath(m, path) {
			return true
		}
	}
	return false
}

// commitShapePlan creates one commit with all writes and deletes via Git Data API.
func (g *ghClient) commitShapePlan(owner, name, branch, message string, plan *ShapePlan) error {
	// GitHub reads a single ref at /git/ref/{ref} and updates one at
	// /git/refs/{ref}. Reusing the read path for the update 404s, which
	// silently defeated shaping: the commit was built and then never landed.
	readRefPath := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, name, ghEsc(branch))
	updateRefPath := fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, name, ghEsc(branch))
	ref, st, err := g.do("GET", readRefPath, nil)
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("%s", ghErr(ref, st))
	}
	obj, _ := ref["object"].(map[string]any)
	baseSHA, _ := obj["sha"].(string)
	if baseSHA == "" {
		return fmt.Errorf("could not resolve branch %s", branch)
	}

	commit, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/git/commits/%s", owner, name, url.PathEscape(baseSHA)), nil)
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("%s", ghErr(commit, st))
	}
	treeObj, _ := commit["tree"].(map[string]any)
	baseTree, _ := treeObj["sha"].(string)
	if baseTree == "" {
		return fmt.Errorf("commit missing tree")
	}

	var tree []map[string]any
	for path, content := range plan.Writes {
		tree = append(tree, map[string]any{
			"path":    path,
			"mode":    "100644",
			"type":    "blob",
			"content": content,
		})
	}
	for _, path := range plan.Deletes {
		tree = append(tree, map[string]any{
			"path": path,
			"mode": "100644",
			"type": "blob",
			"sha":  nil,
		})
	}
	if len(tree) == 0 {
		return nil
	}

	newTree, st, err := g.do("POST", fmt.Sprintf("/repos/%s/%s/git/trees", owner, name), map[string]any{
		"base_tree": baseTree,
		"tree":      tree,
	})
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("create tree: %s", ghErr(newTree, st))
	}
	treeSHA, _ := newTree["sha"].(string)
	if treeSHA == "" {
		return fmt.Errorf("create tree: empty sha")
	}

	newCommit, st, err := g.do("POST", fmt.Sprintf("/repos/%s/%s/git/commits", owner, name), map[string]any{
		"message": message,
		"tree":    treeSHA,
		"parents": []string{baseSHA},
	})
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("create commit: %s", ghErr(newCommit, st))
	}
	commitSHA, _ := newCommit["sha"].(string)
	if commitSHA == "" {
		return fmt.Errorf("create commit: empty sha")
	}

	upd, st, err := g.do("PATCH", updateRefPath, map[string]any{"sha": commitSHA})
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("update ref: %s", ghErr(upd, st))
	}
	return nil
}

// getFileContent is a small helper for tests / identity-only paths.
func (g *ghClient) getFileContent(owner, name, path, ref string) (string, string, error) {
	q := ""
	if ref != "" {
		q = "?ref=" + url.QueryEscape(ref)
	}
	m, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s%s", owner, name, ghEsc(path), q), nil)
	if err != nil {
		return "", "", err
	}
	if st >= 400 {
		return "", "", fmt.Errorf("%s", ghErr(m, st))
	}
	sha, _ := m["sha"].(string)
	encoded, _ := m["content"].(string)
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(encoded, "\n", ""))
	if err != nil {
		return "", "", err
	}
	return string(raw), sha, nil
}
