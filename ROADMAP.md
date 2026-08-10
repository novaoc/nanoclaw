# Roadmap — what's next after 2.0

2.0 shipped the social layer: the willingness model (ambient chime-ins),
human-paced multi-message replies, per-person impressions, and per-channel
expression learning. This file tracks what's designed but not yet built,
roughly in order of value-per-complexity on a 256 MB board.

## Near — cheap, flat-file, high visible payoff

- **Mood as a decaying one-liner.** A per-channel free-text mood ("feeling
  smug about that eval bet") updated with small probability per message,
  regressing to calm after a few minutes of silence, injected into the
  prompt. ~60 lines.
- **Emoji reactions as a social action.** Let ambient turns choose to just
  react (🔥/💀/custom server emoji by name) instead of writing a message —
  the cheapest possible "she saw it" signal.
- **Time-based talk value.** Chattier evenings, silent nights (board-local
  time). Trivial now that TalkValue exists.
- **Interest-driven focus.** When a channel's willingness saturates
  repeatedly (a hot conversation she keeps joining), temporarily raise her
  presence there — shorter backoff, no coin flip for a few minutes — then
  cool down.

- **Holodeck backends.** The demo sandbox (shipped) is static-only by
  design. If demos ever need a server side, the safe path is per-app
  containers with no network egress and hard CPU/RAM caps — a real project,
  not a tweak. Until then, backends belong in the repo with a README.

## Mid — a day or two each

- **Concept-graph memory.** Replace flat MEMORY.md retrieval with a tiny
  graph: nodes = topics with attached memory snippets, edges = co-occurrence
  with integer strength; built by a nightly model pass over sampled chatter,
  retrieved by keyword spreading-activation; forgetting = untouched edges
  lose strength. One gob file, no embeddings (token overlap instead). Also
  upgrades the ambient interest signal for free.
- **Conversation summaries (mid-term memory).** Per-channel rolling digests
  written when a burst of chatter closes, so "what were we arguing about
  this morning?" works without replaying 80 raw messages.
- **Daily schedule.** One model call each morning writes Vela's "day" (what
  she's tinkering with); referenced in prompts so quiet hours and current
  obsessions feel consistent.
- **Night consolidation.** The board is idle at night — a 3 AM pass that
  merges near-duplicate memories, decays stale impressions, and prunes
  expression files.

## Far — heavy infra the feel doesn't need

- Vector/embedding retrieval — needs an embedding model and index; token
  overlap covers a private server fine.
- Full knowledge-graph RAG over ingested documents.
- Subprocess plugin runtime with RPC isolation.
- Auto-collecting and describing the server's stickers/memes for use in
  replies — fun, but each sticker costs a vision call; revisit if a free
  local VLM lands on RISC-V.

## Jargon dictionary (undecided)

Expression learning captures tone but not vocabulary semantics; a tiny
`jargon.json` (term → meaning, learned the same nightly way) might be worth
it once expression learning proves itself. Same guardrails: statistical,
never from one occurrence, no slurs.
