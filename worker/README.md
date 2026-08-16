# vela-worker

The optional build worker for [Vela](https://github.com/novaoc/vela).

Vela's board is a 256 MB RISC-V computer. It is a fine place to hold a
conversation and a poor place to run a Rails test suite, so a build used to
occupy her entire turn — up to 45 minutes during which she could not answer
anyone. This daemon moves that work to a real machine.

With the worker configured, a build becomes a hand-off: Vela frames the
request, forks the foundation, signs a job ticket, enqueues it here, and **her
turn ends**. She stays conversational while the worker clones the repository,
implements the specification, runs the test suite *locally*, pushes commits,
and verifies on Holodex. A detached poller on the board narrates progress into
the request thread and deploys the result when verification passes.

Running the suite locally is the second, quieter win: a mistake surfaces in
seconds instead of through a two-to-four minute remote build.

## Trust split

The worker is the least trusted component in the system, deliberately.

| | Codes & pushes | Proves | Signs deploys |
|---|:--:|:--:|:--:|
| **worker** | ✅ | — | — |
| **Holodex** | — | ✅ | — |
| **Vela (board)** | — | — | ✅ |

It holds **no Holodex build secret**. It verifies using a job *ticket* Vela
signs at enqueue — bound to one repository, with a verification budget and an
expiry. A compromised worker can push bad code and burn its budget; it cannot
ship anything, because a deploy requires Vela's own signature over a receipt
Holodex issued.

Its GitHub token is scoped to *contents: write* on the app repositories — it
cannot create repositories, open pull requests, or change settings.

## The sandbox is load-bearing

The worker runs a coding model against a specification written by whoever
opened the request. That text is untrusted, so:

- every belt path is jailed to one job's workspace — absolute paths, `..`
  traversal, and symlinks pointing outside are all refused;
- `runShell` passes a **stripped environment**: no model key, no GitHub token,
  no Holodex bearer. The daemon holds those; the model's shell never sees them.

`belt_test.go` proves both. That is what keeps a hostile specification a
failed build rather than a host shell.

## Configuration

| Variable | Meaning |
|---|---|
| `WORKER_TOKEN` | bearer for this API (required) |
| `WORKER_ADDR` | listen address, default `:8790` |
| `WORKER_DATA` | job workspaces root, default `/work` |
| `DEEPSEEK_API_KEY` | model key — spend-capped and worker-only |
| `DEEPSEEK_API_URL` | OpenAI-compatible base URL |
| `WORKER_MODEL` | model id |
| `GITHUB_TOKEN` | scoped push token for generated repos |
| `HOLODEX_URL` | e.g. `https://api.example.com` |
| `HOLODEX_TOKEN` | Holodex bearer — **not** the build secret |

On the Vela side, point her at it:

```bash
VELA_WORKER_URL=https://api.example.com/worker
VELA_WORKER_TOKEN=<same as WORKER_TOKEN>
```

## API

```
POST /jobs        {job_id, repo, name, ticket, spec, instructions, port} → 202 {id, state}
GET  /jobs/{id}   → {state, detail, sha, receipt, verifies_used, …}
GET  /healthz
```

`state` moves `queued → coding → verifying → verified | failed`. One live
build per repository: enqueueing a repository that is already building returns
the job already in flight rather than starting a duplicate.

## Running it

Build the image from the repository root (the build stage needs the whole
module, not just this directory):

```bash
docker build -f worker/Dockerfile -t vela-worker .

docker run -d --name vela-worker --restart unless-stopped \
  --env-file worker.env \
  --memory 4g --cpus 2 \
  -v vela-worker-work:/work \
  vela-worker
```

Give it its own container with **no Docker socket**, no Holodex build secret,
and egress allowed — it needs git, RubyGems, and the model API. The app
containers Holodex runs are the exact opposite: no egress at all.

> **Redeploying:** `docker restart` keeps the image the container was created
> from, so a rebuild alone changes nothing. Always `docker rm -f vela-worker`
> and `docker run` again, then confirm the binary actually moved:
> ```bash
> docker exec vela-worker ls -la /usr/local/bin/vela-worker
> ```
