package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCoder(t *testing.T) (*ToolCtx, string) {
	t.Helper()
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"dev": true}
	ws := t.TempDir()
	cfg.Workspace = ws
	return &ToolCtx{cfg: cfg, authorID: "dev"}, ws
}

func TestApplyPatchTable(t *testing.T) {
	cases := []struct {
		name      string
		initial   string
		ops       []patchOp
		want      string
		wantErr   string
		wantFile  string // if non-empty, file must still equal this after failure
		path      string
	}{
		{
			name:    "replace unique",
			initial: "alpha\nbeta\ngamma\n",
			ops:     []patchOp{{Op: "replace", Find: "beta", Replace: "BETA"}},
			want:    "patched",
			wantFile: "alpha\nBETA\ngamma\n",
		},
		{
			name:     "ambiguous match refused",
			initial:  "foo\nbar\nfoo\n",
			ops:      []patchOp{{Op: "replace", Find: "foo", Replace: "FOO"}},
			wantErr:  "matched 2 times",
			wantFile: "foo\nbar\nfoo\n",
		},
		{
			name:     "zero match refused",
			initial:  "only this\n",
			ops:      []patchOp{{Op: "replace", Find: "missing", Replace: "x"}},
			wantErr:  "matched 0 times",
			wantFile: "only this\n",
		},
		{
			name:    "insert_after",
			initial: "a\nb\n",
			ops:     []patchOp{{Op: "insert_after", Find: "a\n", Text: "mid\n"}},
			want:    "patched",
			wantFile: "a\nmid\nb\n",
		},
		{
			name:    "insert_before",
			initial: "a\nb\n",
			ops:     []patchOp{{Op: "insert_before", Find: "b\n", Text: "mid\n"}},
			want:    "patched",
			wantFile: "a\nmid\nb\n",
		},
		{
			name:    "delete region",
			initial: "keep\ndrop me\nkeep2\n",
			ops:     []patchOp{{Op: "delete", Find: "drop me\n"}},
			want:    "patched",
			wantFile: "keep\nkeep2\n",
		},
		{
			name:    "multi-op success",
			initial: "one\ntwo\nthree\n",
			ops: []patchOp{
				{Op: "replace", Find: "one", Replace: "ONE"},
				{Op: "insert_after", Find: "two\n", Text: "two-and-half\n"},
			},
			want:     "patched",
			wantFile: "ONE\ntwo\ntwo-and-half\nthree\n",
		},
		{
			name:    "multi-op all-or-nothing on later failure",
			initial: "one\ntwo\nthree\n",
			ops: []patchOp{
				{Op: "replace", Find: "one", Replace: "ONE"},
				{Op: "replace", Find: "nope", Replace: "x"},
			},
			wantErr:  "matched 0 times",
			wantFile: "one\ntwo\nthree\n",
		},
		{
			name:    "path traversal refused",
			initial: "safe\n",
			path:    "../../etc/evil",
			ops:     []patchOp{{Op: "replace", Find: "safe", Replace: "x"}},
			wantErr: "escapes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coder, ws := testCoder(t)
			rel := tc.path
			if rel == "" {
				rel = "app/sample.rb"
			}
			if !strings.Contains(rel, "..") {
				full := filepath.Join(ws, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(tc.initial), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				// still seed a workspace file so we can prove it wasn't touched
				_ = os.WriteFile(filepath.Join(ws, "safe.txt"), []byte(tc.initial), 0o644)
			}

			out := coder.applyPatch(rel, tc.ops)
			if tc.wantErr != "" {
				if !strings.Contains(out, tc.wantErr) {
					t.Fatalf("out = %q, want error containing %q", out, tc.wantErr)
				}
			} else if !strings.Contains(out, tc.want) {
				t.Fatalf("out = %q, want containing %q", out, tc.want)
			}

			if tc.wantFile != "" && !strings.Contains(rel, "..") {
				got, err := os.ReadFile(filepath.Join(ws, rel))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.wantFile {
					t.Fatalf("file = %q, want %q", got, tc.wantFile)
				}
			}
			if strings.Contains(rel, "..") {
				if _, err := os.Stat(filepath.Join(ws, "..", "..", "etc", "evil")); err == nil {
					t.Fatal("traversal wrote outside workspace")
				}
			}
		})
	}
}

func TestApplyPatchViaRun(t *testing.T) {
	coder, ws := testCoder(t)
	path := filepath.Join(ws, "x.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := coder.Run("apply_patch", `{"path":"x.txt","ops":[{"op":"replace","find":"world","replace":"vela"}]}`)
	if !strings.Contains(out, "patched") || !strings.Contains(out, "+") {
		t.Fatalf("unexpected: %s", out)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello vela\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSearchCodeTable(t *testing.T) {
	coder, ws := testCoder(t)
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/models/user.rb", "class User\n  def create\n  end\nend\n")
	write("app/controllers/users_controller.rb", "class UsersController\n  def create\n  end\nend\n")
	write("README.md", "create docs here\n")
	write("node_modules/pkg/index.js", "function create() {}\n")
	write("vendor/bundle/gem.rb", "def create\nend\n")
	write(".git/config", "create = true\n")
	write("tmp/cache.txt", "create cache\n")
	write("log/dev.log", "create log line\n")
	write("bin/blob.dat", "create\x00binary\n")

	t.Run("finds matches", func(t *testing.T) {
		out := coder.searchCode(`def create`, "", "")
		if !strings.Contains(out, "app/models/user.rb:2:") {
			t.Fatalf("missing model hit: %s", out)
		}
		if !strings.Contains(out, "app/controllers/users_controller.rb:2:") {
			t.Fatalf("missing controller hit: %s", out)
		}
		out = coder.searchCode(`create docs`, "", "")
		if !strings.Contains(out, "README.md:1:") {
			t.Fatalf("missing readme hit: %s", out)
		}
	})

	t.Run("respects path filter", func(t *testing.T) {
		out := coder.searchCode(`def create`, "app/models", "")
		if !strings.Contains(out, "app/models/user.rb") {
			t.Fatalf("expected model hit: %s", out)
		}
		if strings.Contains(out, "controllers") || strings.Contains(out, "README") {
			t.Fatalf("path filter leaked: %s", out)
		}
	})

	t.Run("respects glob filter", func(t *testing.T) {
		out := coder.searchCode(`create`, "", "*.rb")
		if !strings.Contains(out, ".rb:") {
			t.Fatalf("expected rb hits: %s", out)
		}
		if strings.Contains(out, "README.md") {
			t.Fatalf("glob should exclude md: %s", out)
		}
	})

	t.Run("skips excluded dirs and binaries", func(t *testing.T) {
		out := coder.searchCode(`create`, "", "")
		for _, bad := range []string{"node_modules", "vendor/", ".git/", "tmp/", "log/", "blob.dat"} {
			if strings.Contains(out, bad) {
				t.Fatalf("should skip %s, got: %s", bad, out)
			}
		}
	})

	t.Run("reports truncation", func(t *testing.T) {
		var body strings.Builder
		for i := 0; i < maxSearchResults+10; i++ {
			body.WriteString("needle line\n")
		}
		write("flood.txt", body.String())
		out := coder.searchCode(`needle`, "flood.txt", "")
		if !strings.Contains(out, "truncated") {
			t.Fatalf("expected truncation note: %s", out)
		}
		// exactly cap lines of hits (plus maybe the note)
		hits := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "flood.txt:") {
				hits++
			}
		}
		if hits != maxSearchResults {
			t.Fatalf("hits = %d, want %d", hits, maxSearchResults)
		}
	})

	t.Run("path traversal refused", func(t *testing.T) {
		out := coder.searchCode(`x`, "../..", "")
		if !strings.Contains(out, "escapes") {
			t.Fatalf("expected escape error: %s", out)
		}
	})
}

func TestLineDiffCounts(t *testing.T) {
	cases := []struct {
		name    string
		before  string
		after   string
		added   int
		removed int
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, 0},
		{"add lines", "a\n", "a\nb\nc\n", 2, 0},
		{"delete lines", "a\nb\nc\n", "a\n", 0, 2},
		{"modify line", "a\nold\nc\n", "a\nnew\nc\n", 1, 1},
		{"empty to content", "", "x\ny\n", 2, 0},
		{"content to empty", "x\ny\n", "", 0, 2},
		{"reorder", "a\nb\n", "b\na\n", 1, 1}, // LCS keeps two lines; one each side churns
		{"add and delete", "keep\ngone\n", "keep\nnew\n", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			add, del := lineDiffCounts(tc.before, tc.after)
			if add != tc.added || del != tc.removed {
				t.Fatalf("got +%d -%d, want +%d -%d", add, del, tc.added, tc.removed)
			}
		})
	}
}

func TestWriteFileDiffSummary(t *testing.T) {
	coder, ws := testCoder(t)
	// create
	out := coder.writeWorkspaceFile("a.txt", "one\ntwo\n")
	if !strings.Contains(out, "wrote a.txt") || !strings.Contains(out, "+2 -0 lines") {
		t.Fatalf("create summary: %s", out)
	}
	// modify one line
	out = coder.writeWorkspaceFile("a.txt", "one\nTWO\n")
	if !strings.Contains(out, "+1 -1 lines") {
		t.Fatalf("modify summary: %s", out)
	}
	// delete a line
	out = coder.writeWorkspaceFile("a.txt", "one\n")
	if !strings.Contains(out, "+0 -1 lines") {
		t.Fatalf("delete summary: %s", out)
	}
	// apply_patch summary
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = coder.applyPatch("b.txt", []patchOp{{Op: "replace", Find: "y", Replace: "Y\nZ"}})
	if !strings.Contains(out, "patched b.txt") || !strings.Contains(out, "lines") {
		t.Fatalf("patch summary: %s", out)
	}
}

func TestApplyOpsUnit(t *testing.T) {
	_, err := applyOps("aa", []patchOp{{Op: "replace", Find: "a", Replace: "b"}})
	if err == nil || !strings.Contains(err.Error(), "matched 2 times") {
		t.Fatalf("want ambiguous error, got %v", err)
	}
	_, err = applyOps("z", []patchOp{{Op: "replace", Find: "", Replace: "b"}})
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("want empty-find error, got %v", err)
	}
}
