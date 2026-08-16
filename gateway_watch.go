package main

// The 2026-08-15 zombie session: the gateway websocket stayed transport-alive
// for 18 hours — reads waking, heartbeats sent and ACKed, so discordgo never
// reconnected — while Discord delivered zero application events. Vela looked
// online and heard nothing. Heartbeat ACKs cannot detect that failure mode;
// only the event stream can. So the watchdog measures real dispatched events,
// and when the stream has been quiet too long it runs an end-to-end
// receive-path probe: RequestGuildMembers must come back as a
// GuildMembersChunk *event*. No answer → the session is deaf → force a full
// Close/Open (a fresh identify, not a resume of the dead session).

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	// A busy server refreshes lastEvent constantly (messages, presence,
	// typing). Ten quiet minutes is merely "maybe nobody's around" — that's
	// why staleness triggers a probe, never a reconnect directly.
	gatewayStaleAfter = 10 * time.Minute
	// A healthy session answers the probe in a second or two.
	gatewayProbeWait  = 45 * time.Second
	gatewayCheckEvery = 2 * time.Minute
)

// markEvent is a catch-all handler. The signature matters: discordgo's
// catch-all is func(*Session, interface{}) — a *discordgo.Event handler only
// fires for raw/unknown events and misses every typed dispatch including
// READY (verified empirically on the board with gwprobe, 2026-08-16).
func (b *Bot) markEvent(_ *discordgo.Session, _ interface{}) {
	b.lastEvent.Store(time.Now().UnixNano())
}

func (b *Bot) sinceLastEvent() time.Duration {
	return time.Duration(time.Now().UnixNano() - b.lastEvent.Load())
}

func (b *Bot) watchGateway() {
	b.lastEvent.Store(time.Now().UnixNano())
	var lastForced time.Time
	ticker := time.NewTicker(gatewayCheckEvery)
	defer ticker.Stop()
	for range ticker.C {
		if atomic.LoadInt32(&b.draining) != 0 {
			return
		}
		if b.sinceLastEvent() < gatewayStaleAfter {
			continue
		}

		quiet := b.sinceLastEvent()
		guildID, selfID := "", ""
		b.session.State.RLock()
		if len(b.session.State.Guilds) > 0 {
			guildID = b.session.State.Guilds[0].ID
		}
		if b.session.State.User != nil {
			selfID = b.session.State.User.ID
		}
		b.session.State.RUnlock()

		if guildID != "" && selfID != "" {
			// Request our own member entry: a user_ids request is answered
			// without the privileged GuildMembers intent (empty-query requests
			// are silently ignored — gwprobe verified both on the board).
			// The chunk response is itself a dispatched event, so a live
			// session refreshes lastEvent while we wait.
			if err := b.session.RequestGuildMembersList(guildID, []string{selfID}, 1, "vela-liveness", false); err != nil {
				log.Printf("gateway probe send failed after %s quiet: %v", quiet.Round(time.Second), err)
			}
			time.Sleep(gatewayProbeWait)
			if b.sinceLastEvent() < gatewayProbeWait+time.Second {
				continue // probe answered — just a quiet room
			}
		}

		// Churn guard: a deaf verdict can be wrong (a probe regression looks
		// identical), and identify-cycling every check period would hammer
		// the gateway all night. One forced session per half hour.
		if since := time.Since(lastForced); since < 30*time.Minute {
			log.Printf("gateway still quiet (%s) — next forced session in %s", quiet.Round(time.Second), (30*time.Minute - since).Round(time.Second))
			continue
		}
		lastForced = time.Now()
		log.Printf("gateway deaf: no events for %s and probe unanswered — forcing a fresh session", quiet.Round(time.Second))
		b.reconnectGateway()
	}
}

// reconnectGateway tears the session down and identifies fresh. Retries
// forever with backoff: on this box a dead gateway means Vela is simply
// offline, so there is nothing better to do than keep trying.
func (b *Bot) reconnectGateway() {
	_ = b.session.Close()
	delay := 5 * time.Second
	for {
		if atomic.LoadInt32(&b.draining) != 0 {
			return
		}
		if err := b.session.Open(); err != nil {
			log.Printf("gateway reopen failed, retrying in %s: %v", delay, err)
			time.Sleep(delay)
			if delay *= 2; delay > 2*time.Minute {
				delay = 2 * time.Minute
			}
			continue
		}
		b.lastEvent.Store(time.Now().UnixNano())
		log.Printf("gateway reopened with a fresh session")
		return
	}
}
