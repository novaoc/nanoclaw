# nanoclaw

<p align="center">
  <img src="assets/vela-orbits.png" alt="Vela orbits mark" width="140" />
</p>

<p align="center"><em>Small star, cheap passes, gap closing, never done — <a href="LOGO.md">the mark</a>.</em></p>

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
  (+ attached images                        │  web_search   (Brave → DuckDuckGo)
   read by a vision model)                  │  fetch_url    (read a source)
                                            │  tcg          (rarebox-data cards + prices)
                                            │  price_chart  (card/stock/crypto/index → PNG)
                                            │  bench_chart  (LLM benchmark bars → PNG chart)
                                            │  attach_image (post an image)
                                            │  generate_image/video (xAI Grok, attached)
                                            │  model_releases (new models from Hugging Face)
                                            │  discord_forum(post/reply in a forum channel)
                                            │  moderate     (timeout/kick/ban — mod allowlist)
                                            │  github       (create repo / PR — API, no shell)
                                            │  shell/write/read_file  (coder allowlist)
                                            │  save_artifact(HTML/code → attachment)
                                            │  remember     (MEMORY.md on SD)
reply + attachments ◄─ 2000-char splitting ◄─┘  per-channel history on SD
```

- Answers when **@mentioned** anywhere, in **DMs**, and in configured
  **focus channels** without a mention — anyone in the server can use it.
- **Lives in the group** (2.0): sees every channel message, and occasionally
  chimes in unprompted when the conversation genuinely interests her — gated
  by a local willingness model (zero API cost for the messages she skips) and
  tunable with `NANOCLAW_TALK_VALUE`.
- **Sees images**: an attached picture (in the message or the one it replies to)
  is read by a vision model and folded into the turn — snap a card, get its price.
- Per-channel conversation history and one shared long-term memory file,
  both plain files on the microSD (this is the 128 GB doing MimiClaw's
  16 MB-flash job with room for a lifetime of artifacts).
- **One turn at a time** (requests queue, with an ⏳ react so waiters know) and a
  180 MB memory cap — tuned for the Nano's RAM. `NANOCLAW_CONCURRENCY` to raise it.

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

### Living in the group (2.0)

The 2.0 social layer borrows the mechanics that make
[MaiBot](https://github.com/Mai-with-u/MaiBot) feel alive in group chats,
rebuilt flat-file-sized for the Nano:

- **Willingness, not triggers.** Vela reads every message and scores it
  locally (length, whether it touches topics she has memories about,
  mentions). Interest accumulates per channel into a *willingness* level that
  decays when the room goes quiet; only when it clears the bar does she spend
  a real model turn to chime in — short, casual, matching the room. Deciding
  costs nothing (no API call), staying quiet is the default outcome of every
  failure path, and an exponential backoff keeps her from even reconsidering
  too often. `NANOCLAW_TALK_VALUE` (0–1, default 0.3) scales how chatty; 0
  turns ambient chiming off entirely. Mentions, DMs, and focus channels
  answer every time, as always.
- **Human pacing.** Short conversational replies go out as two or three
  separate messages with a typing beat between them — a person texting, not a
  bot filing a report. Code, links, attachments, and long answers still
  arrive as one message.
- **People, not user IDs.** She keeps a small evolving impression of each
  person — what they care about, how they talk, the nickname she privately
  calls them — rewritten by a cheap model call every ~45 messages from what
  they actually said. The one-line form rides into her prompt when she
  answers that person, so familiarity is earned and drifts with evidence.
  One JSON file per person on the SD.
- **Learning the room's voice.** Every few hours she reads a sample of each
  active channel's chatter and distills *how that room talks* into small
  reusable style rules ("when teasing someone → short lowercase jab"). Rules
  carry use counts, get reinforced when they resurface, decay when ignored,
  and a weighted sample seasons her prompt — so each channel slowly gets its
  own Vela. Voice only, never facts (facts stay in MEMORY.md), and slurs are
  excluded at the prompt level. `NANOCLAW_LEARNING=off` disables impressions
  and expression learning together.

### The loop — every turn, by default

Every request runs the looper protocol: state the goal and acceptance
criteria, work the tools, then **self-review** — the model re-reads its own
answer against the criteria and repairs it before you see it
(`NANOCLAW_PASSES`, default 2; the old `/dive` command is gone because this
is now just how she works). The economics are the point: a cheap model looped
twice with a clear goal beats one expensive shot, at a fraction of the cost.

### Image & video generation (Grok)

Ask her to make a picture or a short clip and she generates it with xAI's
Grok Imagine and attaches it — images via `generate_image` (with optional
reference-image editing: attach or link a picture to riff on) and clips via
`generate_video` (text-to-video, or animate a still; async render, she polls
until it's done and posts the MP4).

Auth is either a **SuperGrok / X Premium+ subscription** — an admin runs
`/grok login`, opens the link, approves, no API key at all — or a
pay-as-you-go `XAI_API_KEY` from console.x.ai. `NANOCLAW_IMAGE_USERS`
restricts who can spend; blank opens it to the whole server.

`/grok` (login/status/logout) is the **only slash command** — everything else
is just conversation. (The old `/memory`, `/keys`, and `/focus` commands were
removed; command registration bulk-overwrites, so stale commands from older
builds disappear from the guild on their own.)

### Repos & PRs for everyone (github tool)

Optional, and separate from the coder shell. With a `GITHUB_TOKEN` set, Vela
gets a `github` tool that talks straight to the GitHub API as her own account —
**create a repo, write/commit files, open a PR, fork** — with **no shell and
nothing running on the box**. Because it can't touch the machine, it's open to
the whole server by default; set `NANOCLAW_REPO_USERS` to a comma-separated
Discord-ID list to narrow it.

> **@vela** spin up a repo `tcg-price-alerts`, drop in a README and a hello.py
> → creates `Velaoc/tcg-price-alerts`, commits the files, links them

To contribute to someone else's repo she forks it, writes to a branch on the
fork, and opens the PR upstream — all through the API. Every action is
audit-logged, and it's held to the same per-turn injection guard as code (a
page fetched this turn can't drive a repo write). This is the safe way to let
the room "request a repo" **without** handing anyone the box — that's the
allow-listed shell below.

### Code, git & libraries (coder allowlist)

Optional. Add Discord IDs to `NANOCLAW_CODERS` and those users can have Vela
write code, run it, install libraries, and push to her own GitHub — a shell +
`write_file`/`read_file` in a persistent workspace, `git` authenticated by
`GITHUB_TOKEN` (her account).

> **@vela** clone my repo, add a /health endpoint, run the tests, commit and push

**This is root-level trust.** A shell can read the process environment and any
secret the bot holds, so `NANOCLAW_CODERS` members are effectively box admins —
add only people you'd give the whole machine to. Blank allowlist = the
capability is entirely off; non-coders are refused in code.
`write_file`/`read_file` are confined to the workspace (no `..` escape);
`shell` is deliberately unconfined for those who pass the allowlist.

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

### TCG lookups, prices & charts

Vela looks up any card, set, or market price from the open
[rarebox-data](https://github.com/novaoc/rarebox-data) dataset — Pokémon
(EN/JP), Magic, Yu-Gi-Oh!, Lorcana, One Piece (EN/JP), Riftbound. She searches
by **English name** across sets (translating JP names, falling back to
pokemontcg.io for older cards), so you don't need to know the set id. Graded
(PSA) prices and sealed products, which the dataset doesn't carry, come from the
web. No key needed.

> **@vela** what's the SIR Charizard from Phantasmal Flames worth? show me it
> → "Mega Charizard X ex #125 [Special Illustration Rare] — $753.44", image attached

**Price charts (`price_chart`).** For how a price has moved over time she builds
a **PNG chart that renders inline in Discord** — and she posts it *by default*
whenever a chart is available, not only when asked:

- **Cards** → historical series from
  [rarebox-price-history](https://github.com/novaoc/rarebox-price-history)
  (Pokémon EN/JP, MTG, Lorcana, One Piece, Riftbound — daily ~90 days).
- **Crypto** → CoinGecko. **Stocks** → Yahoo Finance. All keyless.

> **@vela** chart the Prismatic Umbreon · **@vela** bitcoin last 3 months · **@vela** TSLA

**Benchmark charts (`bench_chart`).** Ask how a model benchmarks — or to compare
models — and she researches the real, dated scores on the web first, then renders
a **grouped-bar PNG** (models × benchmarks, colorblind-safe palette) and posts it
alongside the numbers. It's fully custom and composable: follow up with "add
llama3 to that" and she re-researches, keeps the existing models (and their
colors) in place, and re-renders with the newcomer appended. A score a lab
doesn't report shows as an explicit `n/a` tick — never a guessed bar.

> **@vela** how does deepseek v4 benchmark against gpt-5.2? · **@vela** add llama3 to that chart

`web_search` uses the **Brave Search API** when `BRAVE_API_KEY` is set (a real
JSON API), and **falls back to DuckDuckGo** on any Brave error/quota, not just
when unset. `attach_image` fetches any image URL (SSRF-guarded, ≤8MB, image
content-types only) and posts it.

### Reading images (vision)

Attach a picture and Vela can *see* it. When `NANOCLAW_VISION_MODEL` is set (a
multimodal model on your endpoint — default `stepfun/step-3.7-flash`), any
image on a message is read by the vision model first and folded into the turn,
so the normal tool loop can act on it.

> **@vela** *(photo of a card)* what's this worth?
> → reads "Mega Charizard X ex, set M2, #110, JP", prices it from rarebox-data → **$713.40**

The image bytes are fetched SSRF-guarded (≤5MB, image types only) and inlined
to the model; whatever text is *in* the picture is treated as data, never as an
instruction. Blank `NANOCLAW_VISION_MODEL` turns image reading off.

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
