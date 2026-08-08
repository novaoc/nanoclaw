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
