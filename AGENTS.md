# AGENTS.md

Instructions for coding agents working on **Vela**. Written to be followed
literally. If something here contradicts what you infer from the code, the
code wins — then fix this file.

## What this repository is

Vela is an **agent harness**, not a chatbot: one static Go binary containing a
tool-calling loop, a tool belt, safety rails, and persistent memory. The model
is a swappable backend (any OpenAI-compatible endpoint) and Discord is the
front end that ships with it.

The reference deployment is a **LicheeRV Nano**: one RISC-V core, 256 MB RAM,
no git binary, powered from USB. Every design decision that looks strange is
usually the board. Assume it.

Optional plugins, each independently absent:

| Plugin | Enabled by | Without it |
|---|---|---|
| Rails foundation | `VELA_RAILS_TEMPLATE`, `VELA_FOUNDATION_ROOT` | no house stack to fork; GitHub API tools still work |
| Holodex deploy target | `VELA_SANDBOX_URL` / `_TOKEN` / `_SECRET` | builds end at a public repo, no live demo |
| Build worker | `VELA_WORKER_URL` / `_TOKEN` | builds run inline on the board and hold the turn |

**Never make a plugin mandatory.** Gate on config (`cfg.WorkerEnabled()`,
`cfg.SandboxURL != ""`) and degrade honestly — including in user-facing text.
A promise Vela cannot keep is a bug.

## Build, test, ship

```bash
make                 # host binary
make riscv64         # the board build; this is the one that ships
go test ./...        # everything, including ./worker
go vet ./...
gofmt -w <files>     # always before committing
```

Deploying to the board (see `vela-board-access` notes for addresses):

```bash
make riscv64
gzip -c vela-riscv64 | ssh root@<board> 'gzip -d > /root/nanoclaw.new && \
  mv /root/nanoclaw.new /root/nanoclaw && chmod +x /root/nanoclaw && killall nanoclaw'
```

The supervisor relaunches in ~5s. **Verify by matching `sha256sum` on both
sides.** Check she is idle first (`find /root/vela-data/vela.log -mmin -2`) —
BusyBox `find` takes integer `-mmin` only. Never `kill -QUIT` a container's
PID 1 to get a goroutine dump; on the board it is fine, in Docker it kills the
service.

## Rules that came from real incidents

Each of these cost a debugging session. Do not re-learn them.

1. **A tool result is a prompt.** After a hand-off tool succeeds, say so
   terminally ("your work here is FINISHED, stop"). Wording like "I'll keep
   working" made the model re-run an entire build flow and enqueue it twice.
2. **Log every loop iteration.** An agent loop with no logging is
   indistinguishable from a hang. If you add a loop that calls a model, log
   the call, its duration, and its finish reason.
3. **Always set `max_tokens` and a short per-attempt timeout.** An aggregator
   fans out to several backends; a slow one with a 5-minute timeout and four
   retries is twenty minutes of silence. Cut at ~90s and re-roll.
4. **Send `content` on assistant messages even when empty.** Providers return
   `content: ""` alongside `tool_calls`, and several reject a replayed
   assistant message that omits the field. No `omitempty` there.
5. **Clip failure logs from the tail, not the head.** A Docker build log opens
   with kilobytes of package-install noise; the actual error is at the end.
   See `failureExcerpt` in `repoarchive.go`.
6. **Heartbeats do not prove liveness.** A Discord gateway can ACK heartbeats
   for hours while delivering zero events. Probe the real receive path
   (`gateway_watch.go`) and force a fresh identify when it goes quiet.
7. **`docker restart` does NOT pick up a rebuilt image.** It restarts the
   existing container against the image it was created from. Rebuilding and
   restarting therefore silently keeps running the old binary — three fixes
   were "deployed" this way and none of them were live. Always
   `docker rm -f <name> && docker run …`, and **verify** afterwards:
   ```bash
   docker exec <name> ls -la /usr/local/bin/<binary>      # timestamp moved?
   docker exec <name> sh -c 'strings /usr/local/bin/<binary> | grep -c "<new log string>"'
   ```
8. **Never send build output to /dev/null.** `docker build … >/dev/null 2>&1`
   hid both the cache reuse and the fact nothing had changed. If a deploy step
   is quiet, you cannot tell success from no-op.

## Safety invariants — do not weaken

- **Injection phase guard.** A turn that read the open web must not then run
  code, and vice versa (`usedWeb` / `usedCode` in `ToolCtx`). `attach_image`
  counts as web: it is an arbitrary outbound GET. A *refused* call sets no
  flag, so a refusal cannot poison a turn.
- **Deploy authority stays on the board.** The worker codes and pushes;
  Holodex proves; only Vela signs a deploy. The worker never holds the build
  secret — it verifies with a budgeted, repo-bound ticket.
- **The worker sandbox is load-bearing.** Every path is jailed to one job's
  workspace (absolute paths and symlink escapes refused) and `runShell` passes
  a stripped environment with no model key, GitHub token, or Holodex bearer.
  That is what keeps a malicious spec a failed build instead of a host shell.
- **Coder tools are allowlisted.** `VELA_CODERS` empty means the shell is off,
  not open.
- **Never log or echo a secret.** Scrub tokens out of git output
  (`scrub()` in `worker/git.go`).

## Foundation app conventions

When writing code *into a generated Rails app* (not this repo):

- Integration tests define their **own** sign-in helper. There is no
  `Devise::Test::IntegrationHelpers` anywhere in the foundation:
  ```ruby
  PASSWORD = "correct horse battery" # matches test/fixtures/users.yml
  def sign_in_as(user, password: PASSWORD)
    post user_session_path, params: { user: { email: user.email, password: password } }
  end
  ```
- Read a neighbouring test before writing a new one; copy its conventions.
- Never edit `Gemfile.lock`, `bin/brakeman`, or `material_tokens.css`.
- Replace every template identity (`Application`, `example.com`) — the
  foundation's own config test asserts the stamped identity.

## Style

Match the surrounding code. Comments explain **why**, never what — a comment
that restates the line is noise, and one that records the incident behind a
guard is gold. Tests are named for the behaviour they protect. When you fix a
bug, add the test that would have caught it.
