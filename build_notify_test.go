package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestBuildStartedMessageNoEmoji(t *testing.T) {
	msg := buildStartedMessage("driftline-coffee", 30*time.Minute)
	for _, r := range msg {
		if r > unicode.MaxASCII || (r >= 0x2190 && r <= 0x2BFF) {
			// plain ASCII text only — no emoji / pictographs
		}
		if r >= 0x1F300 {
			t.Fatalf("emoji in message: %q", msg)
		}
	}
	if strings.ContainsAny(msg, "😀🎉✅⚡🔧🚀") {
		t.Fatalf("emoji in message: %q", msg)
	}
}

func TestBuildStartedMessageMatchesTurnDeadline(t *testing.T) {
	// Assert minutes against turnDeadline itself — not a copied constant.
	cases := []struct {
		iters, passes int
		coder         bool
	}{
		{20, 1, true},  // coder default: clamps to 30m
		{8, 1, false},  // non-coder: 8*30s=4m
		{2, 1, false},  // floor at 3m
		{32, 2, true},  // dive-scale coder: clamps to 30m
		{4, 1, true},   // coder small: 4*90s=6m
	}
	for _, c := range cases {
		dl := turnDeadline(c.iters, c.passes, c.coder)
		msg := buildStartedMessage("demo-app", dl)
		want := int((dl + time.Minute - 1) / time.Minute)
		if want < 1 {
			want = 1
		}
		needle := fmt.Sprintf("up to about %d minutes", want)
		if !strings.Contains(msg, needle) {
			t.Errorf("iters=%d passes=%d coder=%v dl=%s: want %q in %q",
				c.iters, c.passes, c.coder, dl, needle, msg)
		}
		if !strings.Contains(msg, "demo-app") {
			t.Errorf("missing app name in %q", msg)
		}
		if !strings.Contains(msg, "repository and demo") {
			t.Errorf("missing delivery promise in %q", msg)
		}
	}
}

func TestBuildStartedMessageTracksDeadlineInputChanges(t *testing.T) {
	// Changing the deadline inputs must change the stated minutes.
	short := turnDeadline(2, 1, false) // 3m floor
	long := turnDeadline(20, 1, true)  // 30m ceiling
	if short >= long {
		t.Fatalf("expected short < long, got %s vs %s", short, long)
	}
	ms := buildStartedMessage("x", short)
	ml := buildStartedMessage("x", long)
	if ms == ml {
		t.Fatalf("message must change when deadline changes:\n short=%q\n long=%q", ms, ml)
	}
	shortMins := int((short + time.Minute - 1) / time.Minute)
	longMins := int((long + time.Minute - 1) / time.Minute)
	if !strings.Contains(ms, fmt.Sprintf("%d minutes", shortMins)) {
		t.Fatalf("short msg missing %d: %q", shortMins, ms)
	}
	if !strings.Contains(ml, fmt.Sprintf("%d minutes", longMins)) {
		t.Fatalf("long msg missing %d: %q", longMins, ml)
	}
}

func TestNotifyBuildStartedNilSafe(t *testing.T) {
	tc := &ToolCtx{deadline: 30 * time.Minute}
	// must not panic
	tc.notifyBuildStarted("solo-app")
}

func TestNotifyBuildStartedFiresCallback(t *testing.T) {
	var got []string
	dl := turnDeadline(20, 1, true)
	tc := &ToolCtx{
		deadline: dl,
		notify:   func(s string) { got = append(got, s) },
	}
	tc.notifyBuildStarted("cafe")
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d %v", len(got), got)
	}
	wantMins := int((dl + time.Minute - 1) / time.Minute)
	if !strings.Contains(got[0], fmt.Sprintf("up to about %d minutes", wantMins)) {
		t.Fatalf("bad minutes in %q", got[0])
	}
	if !strings.Contains(got[0], "cafe") {
		t.Fatalf("missing name in %q", got[0])
	}
}

func TestCreateRailsAppNotifiesOnceOnSuccess(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	stub := newShapeStub("notify-cafe", fixtureRepoFiles())
	e := newGHTestEnv(t, stub.handle)
	e.tc.cfg.FoundationRoot = root
	e.tc.cfg.RailsTemplate = "velaoc/foundation"
	e.tc.gh = e.g
	e.tc.deadline = turnDeadline(20, 1, true)

	var got []string
	e.tc.notify = func(s string) { got = append(got, s) }

	out := e.tc.runGithub(toolArgs{Action: "create_rails_app", Name: "notify-cafe"})
	if !strings.HasPrefix(out, "created Rails app") {
		t.Fatalf("create failed: %s", out)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly one build message, got %d %v", len(got), got)
	}
	wantMins := int((e.tc.deadline + time.Minute - 1) / time.Minute)
	if !strings.Contains(got[0], fmt.Sprintf("up to about %d minutes", wantMins)) {
		t.Fatalf("minutes not from deadline: %q (deadline %s)", got[0], e.tc.deadline)
	}
	if !strings.Contains(got[0], "notify-cafe") {
		t.Fatalf("missing app name: %q", got[0])
	}
	for _, r := range got[0] {
		if r >= 0x1F300 {
			t.Fatalf("emoji in notify: %q", got[0])
		}
	}
}

func TestCreateRailsAppNoNotifyOnFailure(t *testing.T) {
	var got []string
	cfg := testCfg(t)
	cfg.GitHubToken = "t"
	// no RailsTemplate → fails before generate
	tc := &ToolCtx{
		cfg: cfg, authorID: "dev",
		deadline: turnDeadline(20, 1, true),
		notify:   func(s string) { got = append(got, s) },
	}
	out := tc.runGithub(toolArgs{Action: "create_rails_app", Name: "nope"})
	if strings.HasPrefix(out, "created Rails app") {
		t.Fatalf("expected failure, got %s", out)
	}
	if len(got) != 0 {
		t.Fatalf("must not notify on failed start, got %v", got)
	}

	// template set but generate fails
	cfg.RailsTemplate = "velaoc/foundation"
	stubFail := newGHTestEnv(t, func(w http.ResponseWriter, r *http.Request, e *ghTestEnv) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, 200, map[string]any{"login": "velaoc"})
		case strings.HasSuffix(r.URL.Path, "/generate"):
			writeJSON(w, 500, map[string]any{"message": "boom"})
		default:
			writeJSON(w, 404, map[string]any{"message": "no"})
		}
	})
	got = nil
	stubFail.tc.cfg.RailsTemplate = "velaoc/foundation"
	stubFail.tc.gh = stubFail.g
	stubFail.tc.deadline = turnDeadline(20, 1, true)
	stubFail.tc.notify = func(s string) { got = append(got, s) }
	out = stubFail.tc.runGithub(toolArgs{Action: "create_rails_app", Name: "boom-app"})
	if strings.HasPrefix(out, "created Rails app") {
		t.Fatalf("expected generate failure, got %s", out)
	}
	if len(got) != 0 {
		t.Fatalf("must not notify when generate fails, got %v", got)
	}
}

func TestOrdinaryToolsDoNotNotify(t *testing.T) {
	var got []string
	cfg := testCfg(t)
	cfg.GitHubToken = "t"
	tc := &ToolCtx{
		cfg: cfg, authorID: "dev",
		deadline: turnDeadline(8, 1, false),
		notify:   func(s string) { got = append(got, s) },
	}
	// remember / save_artifact / github list that isn't create_rails_app
	_ = tc.Run("remember", `{"note":"hello"}`)
	_ = tc.Run("save_artifact", `{"name":"x.html","content":"<p>hi</p>"}`)
	// create_repo path (no rails template) still isn't a rails build start message
	// — only create_rails_app success notifies.
	_ = tc.runGithub(toolArgs{Action: "list_tree", Repo: "nope"})
	if len(got) != 0 {
		t.Fatalf("ordinary tools must not fire build notify, got %v", got)
	}
}

func TestCreateRailsAppNilNotifierSafe(t *testing.T) {
	root := writeFullModuleFixture(t, fixtureModules())
	stub := newShapeStub("eval-app", fixtureRepoFiles())
	e := newGHTestEnv(t, stub.handle)
	e.tc.cfg.FoundationRoot = root
	e.tc.cfg.RailsTemplate = "velaoc/foundation"
	e.tc.gh = e.g
	e.tc.deadline = turnDeadline(20, 1, true)
	// notify intentionally nil — eval path
	out := e.tc.runGithub(toolArgs{Action: "create_rails_app", Name: "eval-app"})
	if !strings.HasPrefix(out, "created Rails app") {
		t.Fatalf("create failed under nil notifier: %s", out)
	}
}
