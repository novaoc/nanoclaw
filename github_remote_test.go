package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ghTestEnv stands up an httptest GitHub API stub and a ghClient pointed at it.
// Tracks request paths so tests can assert commit/search call counts.
type ghTestEnv struct {
	srv      *httptest.Server
	g        *ghClient
	tc       *ToolCtx
	puts     atomic.Int32
	gets     atomic.Int32
	requests []string
}

func newGHTestEnv(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, e *ghTestEnv)) *ghTestEnv {
	t.Helper()
	e := &ghTestEnv{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests = append(e.requests, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete:
			e.puts.Add(1)
		default:
			e.gets.Add(1)
		}
		handler(w, r, e)
	}))
	t.Cleanup(e.srv.Close)

	cfg := testCfg(t)
	cfg.GitHubToken = "test-token"
	e.tc = &ToolCtx{cfg: cfg, authorID: "dev"}
	e.tc.ensureGHCache()
	e.g = &ghClient{
		token:  "test-token",
		owner:  "velaoc",
		api:    e.srv.URL,
		client: e.srv.Client(),
		cache:  e.tc.ghCache,
	}
	return e
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// fixtureRepo is a tiny remote tree used by search/patch tests.
func fixtureTree() map[string]any {
	return map[string]any{
		"sha": "tree-sha",
		"tree": []any{
			map[string]any{"path": "app/models/user.rb", "type": "blob", "sha": "sha-user", "size": float64(40)},
			map[string]any{"path": "app/controllers/users_controller.rb", "type": "blob", "sha": "sha-ctrl", "size": float64(50)},
			map[string]any{"path": "README.md", "type": "blob", "sha": "sha-readme", "size": float64(20)},
			map[string]any{"path": "node_modules/pkg/index.js", "type": "blob", "sha": "sha-nm", "size": float64(30)},
			map[string]any{"path": "vendor/bundle/gem.rb", "type": "blob", "sha": "sha-vendor", "size": float64(20)},
			map[string]any{"path": "bin/blob.dat", "type": "blob", "sha": "sha-bin", "size": float64(20)},
			map[string]any{"path": "app/views", "type": "tree", "sha": "sha-tree"},
		},
	}
}

func fixtureBlobs() map[string]string {
	return map[string]string{
		"sha-user":   "class User\n  def create\n  end\nend\n",
		"sha-ctrl":   "class UsersController\n  def create\n  end\nend\n",
		"sha-readme": "create docs here\n",
		"sha-nm":     "function create() {}\n",
		"sha-vendor": "def create\nend\n",
		"sha-bin":    "create\x00binary\n",
	}
}

func TestRemoteSearchCodeTable(t *testing.T) {
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
			sha := strings.TrimPrefix(r.URL.Path, "/repos/velaoc/demo/git/blobs/")
			body, ok := blobs[sha]
			if !ok {
				writeJSON(w, 404, map[string]any{"message": "Not Found"})
				return
			}
			writeJSON(w, 200, map[string]any{
				"sha": sha, "encoding": "base64", "content": b64(body), "size": len(body),
			})
		default:
			writeJSON(w, 404, map[string]any{"message": "unexpected " + r.URL.Path})
		}
	})

	cases := []struct {
		name    string
		pattern string
		path    string
		glob    string
		want    []string
		notWant []string
	}{
		{
			name:    "file:line hits",
			pattern: `def create`,
			want:    []string{"app/models/user.rb:2:", "app/controllers/users_controller.rb:2:"},
		},
		{
			name:    "path prefix filter",
			pattern: `def create`,
			path:    "app/models",
			want:    []string{"app/models/user.rb:2:"},
			notWant: []string{"controllers", "README"},
		},
		{
			name:    "glob filter",
			pattern: `create`,
			glob:    "*.rb",
			want:    []string{".rb:"},
			notWant: []string{"README.md"},
		},
		{
			name:    "skips noise dirs and binaries",
			pattern: `create`,
			notWant: []string{"node_modules", "vendor/", "blob.dat"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := e.g.searchCode("velaoc/demo", tc.pattern, tc.path, tc.glob, "main")
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("missing %q in:\n%s", w, out)
				}
			}
			for _, n := range tc.notWant {
				if strings.Contains(out, n) {
					t.Fatalf("should not contain %q in:\n%s", n, out)
				}
			}
		})
	}

	t.Run("truncation reported", func(t *testing.T) {
		// Flood one file past maxSearchResults.
		var flood strings.Builder
		for i := 0; i < maxSearchResults+10; i++ {
			flood.WriteString("needle line\n")
		}
		blobs["sha-user"] = flood.String()
		// Bust blob cache for this sha.
		delete(e.g.cache.blobs, "sha-user")
		out := e.g.searchCode("velaoc/demo", `needle`, "app/models/user.rb", "", "main")
		if !strings.Contains(out, "truncated") {
			t.Fatalf("expected truncation note: %s", out)
		}
		hits := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "user.rb:") {
				hits++
			}
		}
		if hits != maxSearchResults {
			t.Fatalf("hits=%d want %d", hits, maxSearchResults)
		}
		// restore
		blobs["sha-user"] = fixtureBlobs()["sha-user"]
		delete(e.g.cache.blobs, "sha-user")
	})

	t.Run("tree cached across searches", func(t *testing.T) {
		before := 0
		for _, r := range e.requests {
			if strings.Contains(r, "/git/trees/") {
				before++
			}
		}
		_ = e.g.searchCode("velaoc/demo", `User`, "app/models", "", "main")
		_ = e.g.searchCode("velaoc/demo", `create`, "app/models", "", "main")
		after := 0
		for _, r := range e.requests {
			if strings.Contains(r, "/git/trees/") {
				after++
			}
		}
		if after != before {
			t.Fatalf("tree re-fetched: before=%d after=%d", before, after)
		}
	})
}

func TestRemotePatchFileTable(t *testing.T) {
	type fileState struct {
		content string
		sha     string
	}

	cases := []struct {
		name       string
		initial    string
		ops        []patchOp
		wantErr    string
		wantCommit bool
		wantOut    string
		wantFile   string
	}{
		{
			name:       "ambiguous match refused nothing committed",
			initial:    "foo\nbar\nfoo\n",
			ops:        []patchOp{{Op: "replace", Find: "foo", Replace: "FOO"}},
			wantErr:    "matched 2 times",
			wantCommit: false,
			wantFile:   "foo\nbar\nfoo\n",
		},
		{
			name:       "zero match refused nothing committed",
			initial:    "only this\n",
			ops:        []patchOp{{Op: "replace", Find: "missing", Replace: "x"}},
			wantErr:    "matched 0 times",
			wantCommit: false,
			wantFile:   "only this\n",
		},
		{
			name:    "multi-op success commits once",
			initial: "one\ntwo\nthree\n",
			ops: []patchOp{
				{Op: "replace", Find: "one", Replace: "ONE"},
				{Op: "insert_after", Find: "two\n", Text: "two-and-half\n"},
			},
			wantCommit: true,
			wantOut:    "patched",
			wantFile:   "ONE\ntwo\ntwo-and-half\nthree\n",
		},
		{
			name:    "multi-op later failure commits nothing",
			initial: "one\ntwo\nthree\n",
			ops: []patchOp{
				{Op: "replace", Find: "one", Replace: "ONE"},
				{Op: "replace", Find: "nope", Replace: "x"},
			},
			wantErr:    "matched 0 times",
			wantCommit: false,
			wantFile:   "one\ntwo\nthree\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fileState{content: tc.initial, sha: "sha-v1"}
			var putCount int
			e := newGHTestEnv(t, func(w http.ResponseWriter, r *http.Request, e *ghTestEnv) {
				switch {
				case r.URL.Path == "/user":
					writeJSON(w, 200, map[string]any{"login": "velaoc"})
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
					writeJSON(w, 200, map[string]any{
						"sha": st.sha, "encoding": "base64",
						"content": b64(st.content), "path": "app/sample.rb",
					})
				case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
					putCount++
					body, _ := io.ReadAll(r.Body)
					var payload struct {
						Content string `json:"content"`
						SHA     string `json:"sha"`
						Message string `json:"message"`
					}
					_ = json.Unmarshal(body, &payload)
					if payload.SHA != st.sha {
						writeJSON(w, 409, map[string]any{"message": "sha mismatch"})
						return
					}
					raw, _ := base64.StdEncoding.DecodeString(payload.Content)
					st.content = string(raw)
					st.sha = "sha-v2"
					writeJSON(w, 200, map[string]any{
						"content": map[string]any{"html_url": "https://github.com/velaoc/demo/blob/main/app/sample.rb"},
					})
				default:
					writeJSON(w, 404, map[string]any{"message": "unexpected " + r.Method + " " + r.URL.Path})
				}
			})

			out := e.g.patchFile("velaoc/demo", "app/sample.rb", tc.ops, "test patch", "")
			if tc.wantErr != "" {
				if !strings.Contains(out, tc.wantErr) {
					t.Fatalf("out=%q want err containing %q", out, tc.wantErr)
				}
			} else if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("out=%q want containing %q", out, tc.wantOut)
			}
			if tc.wantCommit {
				if putCount != 1 {
					t.Fatalf("commits=%d want 1; out=%s", putCount, out)
				}
			} else if putCount != 0 {
				t.Fatalf("expected no commit, got %d; out=%s", putCount, out)
			}
			if st.content != tc.wantFile {
				t.Fatalf("remote file=%q want %q", st.content, tc.wantFile)
			}
		})
	}
}

func TestRemoteGithubActionsNoRegression(t *testing.T) {
	e := newGHTestEnv(t, func(w http.ResponseWriter, r *http.Request, e *ghTestEnv) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, 200, map[string]any{"login": "velaoc"})
		case r.URL.Path == "/repos/velaoc/demo":
			writeJSON(w, 200, map[string]any{"default_branch": "main", "html_url": "https://github.com/velaoc/demo"})
		case strings.HasPrefix(r.URL.Path, "/repos/velaoc/demo/git/trees/"):
			writeJSON(w, 200, map[string]any{
				"tree": []any{
					map[string]any{"path": "README.md", "type": "blob", "sha": "s1", "size": float64(5)},
					map[string]any{"path": "app/models/user.rb", "type": "blob", "sha": "s2", "size": float64(10)},
				},
			})
		case strings.Contains(r.URL.Path, "/contents/README.md") && r.Method == http.MethodGet:
			writeJSON(w, 200, map[string]any{
				"sha": "s1", "content": b64("# hi\n"), "encoding": "base64",
			})
		case strings.Contains(r.URL.Path, "/contents/README.md") && r.Method == http.MethodPut:
			writeJSON(w, 200, map[string]any{
				"content": map[string]any{"html_url": "https://github.com/velaoc/demo/blob/main/README.md"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/user/repos":
			writeJSON(w, 201, map[string]any{"html_url": "https://github.com/velaoc/new-app"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			writeJSON(w, 201, map[string]any{"html_url": "https://github.com/velaoc/demo/pull/1"})
		default:
			writeJSON(w, 404, map[string]any{"message": "unexpected " + r.Method + " " + r.URL.Path})
		}
	})

	// Wire runGithub through cfg so allowlist/token gates pass.
	e.tc.cfg.GitHubToken = "test-token"
	// Monkey-patch via direct gh methods for API shape, and runGithub for dispatch.
	// newGH always hits real API base — exercise methods on e.g and dispatch validation on tc.

	if out := e.g.listTree("velaoc/demo", "main", "app"); !strings.Contains(out, "app/models/user.rb") {
		t.Fatalf("list_tree: %s", out)
	}
	if out := e.g.readFiles("velaoc/demo", "main", []string{"README.md"}, 0, 0); !strings.Contains(out, "# hi") {
		t.Fatalf("read_files: %s", out)
	}
	if out := e.g.putFile("velaoc/demo", "README.md", "# bye\n", "update", ""); !strings.Contains(out, "committed") {
		t.Fatalf("put_file: %s", out)
	}
	if out := e.g.createRepo("new-app", "d"); !strings.Contains(out, "created repo") {
		t.Fatalf("create_repo: %s", out)
	}
	if out := e.g.openPR("velaoc/demo", "t", "feature", "main", "body"); !strings.Contains(out, "opened PR") {
		t.Fatalf("open_pr: %s", out)
	}

	// Dispatch recognizes new actions and rejects unknown.
	cfg := testCfg(t)
	cfg.GitHubToken = "t"
	tc := &ToolCtx{cfg: cfg, authorID: "u"}
	// Without a reachable API, search_code still validates args:
	out := tc.runGithub(toolArgs{Action: "search_code"})
	if !strings.Contains(out, "needs repo and pattern") {
		t.Fatalf("search_code validation: %s", out)
	}
	out = tc.runGithub(toolArgs{Action: "patch_file", Repo: "x", Path: "y"})
	if !strings.Contains(out, "ops") {
		t.Fatalf("patch_file validation: %s", out)
	}
	out = tc.runGithub(toolArgs{Action: "not_a_thing"})
	if !strings.Contains(out, "unknown action") || !strings.Contains(out, "search_code") || !strings.Contains(out, "patch_file") {
		t.Fatalf("unknown action help: %s", out)
	}
}

func TestGithubToolDescriptionMentionsSearchAndPatch(t *testing.T) {
	cfg := testCfg(t)
	cfg.GitHubToken = "t"
	var desc, params string
	for _, d := range toolDefs(cfg) {
		if d.Function.Name == "github" {
			desc = d.Function.Description
			params = string(d.Function.Parameters)
		}
	}
	for _, want := range []string{
		"search_code",
		"patch_file",
		"SEARCH BEFORE",
		"Prefer for small edits",
		"REPLACES THE ENTIRE FILE",
		"start_line",
		"never fetch_url",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("description missing %q", want)
		}
	}
	if !strings.Contains(params, "search_code") || !strings.Contains(params, "patch_file") || !strings.Contains(params, `"pattern"`) {
		t.Fatalf("params schema incomplete: %s", params)
	}
	if !strings.Contains(params, "start_line") || !strings.Contains(params, "end_line") {
		t.Fatalf("params missing line range: %s", params)
	}
}

func TestGithubReadFilesRangedAndPerFileBudget(t *testing.T) {
	// Large first file + two small ones: multi-file budget is per path so small
	// files still appear in full; large file gets an honest continuation hint.
	var large strings.Builder
	for i := 1; i <= 300; i++ {
		large.WriteString(strings.Repeat("R", 80))
		large.WriteByte('\n')
	}
	files := map[string]string{
		"config/routes.rb": large.String(),
		"a.rb":             "alpha-content\n",
		"b.rb":             "beta-content\n",
	}
	e := newGHTestEnv(t, func(w http.ResponseWriter, r *http.Request, e *ghTestEnv) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, 200, map[string]any{"login": "velaoc"})
		case r.URL.Path == "/repos/velaoc/demo":
			writeJSON(w, 200, map[string]any{"default_branch": "main"})
		case strings.Contains(r.URL.Path, "/contents/") && r.Method == http.MethodGet:
			path := strings.TrimPrefix(r.URL.Path, "/repos/velaoc/demo/contents/")
			body, ok := files[path]
			if !ok {
				writeJSON(w, 404, map[string]any{"message": "missing " + path})
				return
			}
			writeJSON(w, 200, map[string]any{
				"sha": "s", "content": b64(body), "encoding": "base64",
			})
		default:
			writeJSON(w, 404, map[string]any{"message": "unexpected " + r.Method + " " + r.URL.Path})
		}
	})

	// Ranged read returns exactly the requested lines.
	out := e.g.readFiles("velaoc/demo", "main", []string{"a.rb"}, 0, 0)
	if !strings.Contains(out, "alpha-content") {
		t.Fatalf("small whole file: %s", out)
	}

	// Build a tiny multi-line file for exact range.
	files["range.rb"] = "L1\nL2\nL3\nL4\n"
	out = e.g.readFiles("velaoc/demo", "main", []string{"range.rb"}, 2, 3)
	if !strings.Contains(out, "L2\nL3") || strings.Contains(out, "L1") || strings.Contains(out, "L4") {
		t.Fatalf("ranged github read: %s", out)
	}

	// 3-file: large first must not starve the others.
	out = e.g.readFiles("velaoc/demo", "main", []string{"config/routes.rb", "a.rb", "b.rb"}, 0, 0)
	if !strings.Contains(out, "alpha-content") || !strings.Contains(out, "beta-content") {
		t.Fatalf("multi-file starved small files: %s", out)
	}
	if !strings.Contains(out, "[truncated:") || !strings.Contains(out, "of 300 total") {
		t.Fatalf("large file missing honest truncation: %s", out)
	}
	if !strings.Contains(out, `read_files paths=["config/routes.rb"] start_line=`) {
		t.Fatalf("missing continuation hint: %s", out)
	}

	// Out-of-range fails clearly (not empty).
	out = e.g.readFiles("velaoc/demo", "main", []string{"a.rb"}, 99, 0)
	if !strings.Contains(out, "past end of file") {
		t.Fatalf("OOR: %s", out)
	}
	if strings.TrimSpace(strings.TrimPrefix(out, "=== a.rb ===")) == "" {
		t.Fatalf("OOR must not be empty body: %s", out)
	}
}
