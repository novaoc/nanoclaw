# Roadmap — what's next after 2.0

2.0 shipped the MaiBot-inspired social layer: the willingness model (ambient
chime-ins), human-paced multi-message replies, per-person impressions, and
per-channel expression learning. This file tracks the ideas researched from
[MaiBot](https://github.com/Mai-with-u/MaiBot) that fit Vela but didn't make
2.0, roughly in order of value-per-complexity on a 256 MB board.

## Near — cheap, flat-file, high visible payoff

- **Mood as a decaying one-liner.** A per-channel free-text mood ("feeling
  smug about that eval bet") updated with small probability per message,
  regressing to calm after ~3 minutes of silence, injected into the prompt.
  MaiBot 0.10 `mood_manager.py`. ~60 lines.
- **Emoji reactions as a social action.** Let ambient turns choose to just
  react (🔥/💀/custom server emoji by name) instead of writing a message —
  cheapest possible "she saw it" signal. Needs an action menu in the ambient
  prompt + one Discord call.
- **Time-based talk value.** `talk_value_rules` — chattier evenings, silent
  nights (board-local time). Trivial once TalkValue exists.
- **Interest-driven focus.** When a channel's willingness saturates
  repeatedly (a hot conversation she keeps joining), temporarily raise her
  presence there: shorter backoff, no coin flip for a few minutes, then cool
  down. MaiBot's normal→focus mode transition, minus the separate planner.

## Mid — a day or two each

- **Hippocampus-lite memory graph.** Replace flat MEMORY.md retrieval with a
  tiny concept graph: nodes = topics with attached memory snippets, edges =
  co-occurrence with integer strength; built by a nightly LLM pass over
  sampled chatter (bimodal recent/day-old sampling), retrieved by keyword
  spreading-activation; forgetting = untouched edges lose strength. One gob
  file, no embeddings (token overlap instead). Also upgrades the ambient
  interest signal for free. MaiBot 0.x `Hippocampus.py`.
- **Conversation summaries (mid-term memory).** Per-channel rolling digests
  written when a burst of chatter closes, so "what were we arguing about this
  morning?" works without replaying 80 raw messages.
- **Daily schedule.** One LLM call each morning writes Vela's "day" (what
  she's tinkering with); referenced in prompts so quiet hours and current
  obsessions feel consistent. MaiBot 0.6 `[schedule]`.
- **Night consolidation ("dreams").** The board is idle at night — a 3 AM
  pass that merges near-duplicate memories, decays stale impressions, and
  prunes expression files. MaiBot 0.12 `[dream]`.

## Far / probably never — heavy infra the feel doesn't need

- Vector/embedding retrieval (A_Memorix-style episode store) — needs an
  embedding model and index; token overlap covers a private server fine.
- LPMM-style GraphRAG knowledge base with PageRank retrieval.
- Subprocess plugin runtime with RPC isolation.
- VLM sticker stealing (auto-collecting and describing the server's memes) —
  fun, but each sticker costs a vision call; revisit if a free local VLM
  lands on RISC-V.

## Jargon dictionary (undecided)

MaiBot 1.x learns server slang *with meanings* as a separate store. Vela's
expression learning captures tone but not vocabulary semantics; a tiny
`jargon.json` (term → meaning, learned the same nightly way) might be worth
it once expression learning proves itself. Same guardrails: statistical,
never from one occurrence, no slurs.
