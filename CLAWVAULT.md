# clawvault — key custody, split out of Vela

## Why

Encryption at rest protects a stolen SD card. It protects nothing against
whoever controls the running code, because the process must decrypt keys to
use them — the secret and plaintext keys live in memory. Vela can write and
push code; if that code runs on the Nano, she *is* the process. So the fix is
not "encrypt harder" — it is to break the chain between **can author code** and
**runs with the secret**.

Until that split exists, nanoclaw enforces a hard **interlock**: the shell /
file tools are disabled whenever this process holds `NANOCLAW_SECRET`
(`Config.CodeEnabled` / `CodeInterlockTripped`; startup logs it; the tools
aren't even offered and refuse if called). You get code **or** in-process keys,
never both. clawvault is what lets you have both safely.

## The daemon

A tiny separate binary, its own Unix user, that owns custody end to end:

- Holds `NANOCLAW_SECRET` and the keys dir (`0700`, nanoclaw's user has no read
  access). The secret stays off the microSD (`gen-secret.sh`).
- Exposes only a local Unix socket. The API accepts prompts and returns text —
  **no endpoint ever returns a key.** Ops: `status(uid)`, `read(uid,text)`,
  `queue(uid,text)→token`, `confirm(uid,token)→result`, `disconnect(uid)`.
- Enforces policy **inside itself**, fail-closed: a read allowlist
  (balance/price/portfolio/fees) runs immediately; everything else needs
  confirmation. Plus per-user daily spend caps, rate limits, and an
  append-only audit log.
- **Owns `/connect`.** The `bk_` key is entered through clawvault's *own*
  minimal Discord application, so it never transits nanoclaw's process — if
  nanoclaw took the key first, malicious nanoclaw code could capture it before
  encryption and the split would be theater.
- **Owns confirmation**, out-of-band with its own Discord credentials: it DMs
  the user the button (or a one-time nonce they type back) for the exact
  action, and verifies it itself. The confirm decision stops being anything
  nanoclaw (or its LLM) can set.

After the split, the worst fully-compromised nanoclaw code can do is send
per-user, policy-gated, capped, audited prompts to the socket — it can annoy,
it cannot drain or exfiltrate. Vela talks to the vault; she can never be it.

## Deploy gate (authorship ≠ shipping)

- Vela pushes branches / opens PRs; `main` is branch-protected (human review,
  no force-push, no admin bypass).
- The Nano never pulls `main`. It deploys only from tags **signed with Wren's
  key**; the init script verifies the signature/hash of the binary before
  exec, and this covers `deploy/` too (an edited init script = root code exec).
  Vela doesn't hold the signing key.
- Vela has no SSH to the Nano (`authorized_keys` = Wren's key only) and no
  self-update / self-restart tool. Audit that none is ever added — a
  "redeploy myself" capability is the whole game.
- clawvault lives behind stricter review (own repo or CODEOWNERS): it's small,
  changes rarely, every diff gets a careful read. Security lives in ~300
  auditable lines, not everywhere.

## Box hardening

- systemd per service: `NoNewPrivileges`, `ProtectSystem=strict`,
  `ReadWritePaths=` limited to each service's own data dir.
- nftables egress: clawvault may reach only Bankr's API host; nanoclaw can't
  touch the vault's files.
- Secret in an env file readable only by clawvault's user, off the SD.

## Blast radius (Bankr side)

Users connect keys backed by **small hot wallets**, with whatever scoping /
spend limits Bankr's keys support. clawvault's caps are the backstop when
Bankr's aren't granular enough.

## Honest limits

Root on the Nano still beats all of this, and so does a bad diff that Wren
signs — human review judgment and the signing key are the remaining trust.
What the split buys: Vela's **unilateral** powers (write code, push, even fully
own the nanoclaw process) can no longer read a key, skip a confirmation, or
exceed a cap. The boundary moves from "the model behaved" to "a human signed
it, and a small daemon enforces it."

## Status

Interlock: shipped (code and in-process keys are mutually exclusive). The
daemon, its own `/connect`/confirmation Discord app, deploy signing, and box
hardening are the next build — deliberately staged so the custody layer is
written and reviewed carefully rather than rushed. There is no unsafe window in
the meantime: while keys are in-process, Vela has no shell.
