# nanoclaw

A [MimiClaw](https://github.com/memovai/mimiclaw)-style pocket AI agent — but
where MimiClaw squeezes pure C onto a $5 ESP32-S3, nanoclaw rides a
**LicheeRV Nano** (RISC-V, 256 MB RAM, real Linux) with a 128 GB microSD:
one 6.6 MB static Go binary, **Discord** in, **DeepSeek** agent loop inside,
tools + memory on the SD card. Plug into USB power and it serves your whole
server, 24/7.

Focused on **AI development and agentic engineering**: ask it in Discord to
mock up a website (it attaches a self-contained HTML file), pressure-test a
project idea, dig up benchmarks for a new model release, or review an agent
design — it searches the web, reads sources, cites what it read, and
remembers your server's projects across reboots.

## How it works

```
Discord message ──► gateway (discordgo) ──► agent loop (DeepSeek tool calling)
                                             │  web_search   (DuckDuckGo)
                                             │  fetch_url    (read a source)
                                             │  save_artifact(HTML/code → attachment)
                                             │  remember     (MEMORY.md on SD)
reply + attachments ◄─ 2000-char splitting ◄─┘  per-channel history on SD
```

- Answers when **@mentioned** anywhere, in **DMs**, and in configured
  **focus channels** without a mention — anyone in the server can use it.
- Per-channel conversation history and one shared long-term memory file,
  both plain files on the microSD (this is the 128 GB doing MimiClaw's
  16 MB-flash job with room for a lifetime of artifacts).
- Two concurrent turns max and a 180 MB memory cap — tuned for the Nano.

## Setup

### 1. Create the Discord bot
[discord.com/developers/applications](https://discord.com/developers/applications)
→ New Application → Bot: copy the **token** and enable the
**Message Content intent**. Invite it with the *bot* scope +
*Send Messages / Read Message History / Attach Files* permissions.

### 2. Get a DeepSeek key
[platform.deepseek.com](https://platform.deepseek.com) — `deepseek-chat`
does the tool calling; a heavy day of use costs pennies.

### 3. Prepare the LicheeRV Nano
Flash the official image to the microSD
([wiki](https://wiki.sipeed.com/hardware/en/lichee/RV_Nano/1_intro.html)),
get it on WiFi/Ethernet, note its IP. Make a data dir on the SD's big
partition, e.g. `mkdir -p /mnt/sd/nanoclaw`.

### 4. Build, configure, deploy

```bash
make riscv64                          # cross-compile from any machine with Go
scp nanoclaw-riscv64 root@<nano-ip>:/root/nanoclaw
scp nanoclaw.env.example root@<nano-ip>:/etc/nanoclaw.env
# edit /etc/nanoclaw.env on the board: token, key, FOCUS_CHANNELS, data dir

# buildroot image (SysVinit):
scp deploy/S99nanoclaw root@<nano-ip>:/etc/init.d/ && ssh root@<nano-ip> \
  'chmod +x /etc/init.d/S99nanoclaw && /etc/init.d/S99nanoclaw start'
# systemd image instead:
scp deploy/nanoclaw.service root@<nano-ip>:/etc/systemd/system/ && \
  ssh root@<nano-ip> 'systemctl enable --now nanoclaw'
```

It also runs anywhere else (`make run`) — the Nano is the destination, not a
requirement, so you can dev on a laptop with the same env file.

## Using it

### /dive — the deep-loop skill

For anything that deserves more than a quick answer:

> **/dive** task: compare the latest DeepSeek release against GPT on coding benchmarks — real numbers with dates

`/dive` runs the looper protocol: state the goal and acceptance criteria,
work with a doubled tool budget (16 tool calls), then **self-review** — the
model re-reads its own answer against the criteria and repairs it before you
see it (`NANOCLAW_DIVE_PASSES`, default 2). The economics are the point: a
cheap model looped twice with a clear goal beats one expensive shot, at a
fraction of the cost.

### Tokens & wallet (Bankr)

Optional. Set `NANOCLAW_SECRET` and Vela can design tokens (no key needed) and
run **per-user wallets** on Base through [Bankr](https://bankr.bot) — Bankr owns
the wallet, signs, and executes; nanoclaw never touches private keys.

**Each person connects their own wallet.** `/connect api_key:bk_…` (from
[bankr.bot/api](https://bankr.bot/api), wallet access enabled) — the command
and its reply are **ephemeral**, so only that user ever sees them. Vela then
acts *only* on that person's wallet, recognized by their Discord id; nobody can
touch anyone else's funds. `/disconnect` wipes your key.

> **/connect** api_key: bk_…                          ← private, one time
> **@nanoclaw** what's my portfolio worth?            ← your wallet
> **@nanoclaw** review my token idea for indie shops  ← strategy, no wallet
> **@nanoclaw** launch $SHELF and buy $50 of it       ← your wallet, confirmed

**Custody & guardrails:**
- Connected keys are **AES-GCM encrypted at rest** under `NANOCLAW_SECRET`
  (Discord id as AAD, so a swapped key file can't impersonate another owner).
  Generate the secret on the box with `deploy/gen-secret.sh` — it writes a
  random 256-bit value to a root-600 file that's never printed or committed.
  Keep that file *off the microSD* and a stolen card can't decrypt anything.
  No secret → the whole wallet feature is off and no keys are ever stored.
- Vela acts only on the requester's own connected wallet — no shared wallet,
  no borrowing someone else's.
- **Confirmation is out-of-band, not model-controlled.** A fund-moving or
  deploy request never executes from chat — it queues and the requester gets
  a **Confirm button**. Only their click (verified against their Discord id,
  in Go) runs the transaction. So a prompt-injected web page fetched via a
  tool can't trigger a spend: it can put text in the model's context, but it
  can't click a button as the user.
- Read vs. write is **fail-closed**: only clear look-ups (balance, portfolio,
  price…) run without a click; anything ambiguous ("move", "pay", "yeet")
  routes to the button.
- **SSRF-guarded fetches**: the web tools refuse private/loopback/link-local
  addresses and re-check after every redirect, so a public URL can't bounce
  the bot into your LAN.
- Honest limit: a bot that spends autonomously must be able to decrypt keys at
  runtime, so anyone with live *root on the running box* is outside what
  encryption can stop. This protects against a lost/stolen card and casual
  file access, not a determined operator dumping process memory.

Token strategy follows the five-forces method from Bankr's
[token-strategist](https://github.com/BankrBot/token-strategist); the wallet
API is [bankr-api-examples](https://github.com/BankrBot/bankr-api-examples).

### Code, git & libraries (coder allowlist)

Optional. Add Discord IDs to `NANOCLAW_CODERS` and those users can have Vela
write code, run it, install libraries, and push to her own GitHub — a shell +
`write_file`/`read_file` in a persistent workspace, `git` authenticated by
`GITHUB_TOKEN` (her account).

> **@vela** clone my repo, add a /health endpoint, run the tests, commit and push

**This is root-level trust.** A shell can read the process environment and
every user's encrypted wallet material, so `NANOCLAW_CODERS` members are
effectively box admins — add only people you'd give the whole machine to.
Blank allowlist = the capability is entirely off; non-coders are refused in
code. `write_file`/`read_file` are confined to the workspace (no `..` escape);
`shell` is deliberately unconfined for those who pass the allowlist.

**Custody/execution interlock.** Code and in-process wallet keys are mutually
exclusive: while this process holds `NANOCLAW_SECRET`, the shell/file tools are
**disabled** (not offered, and refused if called) — otherwise a shell could
read the secret and every user's keys. To run both, key custody moves to a
separate process (**[clawvault](CLAWVAULT.md)**) so nanoclaw no longer holds
the secret. That split — plus signed-tag deploys and box hardening — is the
architecture in [CLAWVAULT.md](CLAWVAULT.md); the interlock ships today so
there's no unsafe window.

**Injection guard.** Web fetches (`web_search`/`fetch_url`) and code execution
(`shell`/`write_file`/`read_file`) are mutually exclusive *within a single
turn*: once a turn has touched the web, code is refused for that turn, and vice
versa. A page fetched this turn therefore can't inject the shell commands the
model then runs. Research in one message, run code in the next.

**⚠️ Deploy sequencing — do this before enabling coders in production.** A
coder shell can read `GITHUB_TOKEN` from the environment and push whatever it
likes; only the **signed-tag deploy gate** makes such a push inert (the Nano
runs only binaries you signed). So: **do not set `NANOCLAW_CODERS` in
production until signed-tag verification is live on the box.** And scope the
token to blast radius — a **fine-grained PAT limited to Vela's own repos,
contents + pull-request write only, no admin**, treated as rotatable. Then a
stolen token costs embarrassment, not infrastructure. nanoclaw logs this
reminder at startup when code + a token are both configured.

**Hardware reality — she's a 22×36 mm board, not a build server.** The
LicheeRV Nano is one ~1 GHz RISC-V core + **256 MB RAM**. Great for git, small
scripts, config, and lightweight installs; a big `npm install`, a from-source
compile, or a heavy test suite will crawl or OOM, and RISC-V means some
prebuilt wheels/binaries don't exist. For serious builds she'll write the code
and **push**, letting CI (GitHub Actions) or a bigger machine compile — and
she'll tell you that instead of thrashing the board.

### Mentions — quick turns

> **@nanoclaw** mock up a landing page for a TCG price-alert app — dark, one CTA

→ replies with the reasoning and attaches `landing.html`, self-contained,
open it in any browser.

> **@nanoclaw** what are the benchmark numbers for the latest DeepSeek release vs GPT?

→ searches, reads the sources, answers with cited URLs.

> **@nanoclaw** remember that the booth project ships on the 20th

→ lands in `MEMORY.md`, loaded into every future conversation.

## Development

```bash
make test    # go vet + the offline test suite (agent loop runs against a stub)
make run     # local run with ./nanoclaw.env
```

MIT.
