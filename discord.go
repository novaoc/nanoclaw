package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	cfg     *Config
	agent   *Agent
	session *discordgo.Session
	// global turn semaphore (cap = cfg.Concurrency, default 1) — how many turns
	// run at once, RAM-safe on the Nano; others queue on it.
	locks   chan struct{}
	pending int32 // in-flight + queued turns, to cap a spam pile-up (maxPending)
}

// maxPending bounds queued goroutines so a message flood can't OOM the 128 MB
// board — past this, new messages get a 🚫 react instead of piling up.
const maxPending = 24

func NewBot(cfg *Config, agent *Agent) (*Bot, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent |
		discordgo.IntentsDirectMessages
	b := &Bot{cfg: cfg, agent: agent, session: s, locks: make(chan struct{}, cfg.Concurrency)}
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onGuildCreate)
	s.AddHandler(b.onInteraction)
	return b, nil
}

// /dive — the deep-loop skill: clear goal, bigger tool budget, self-review
// passes. Registered per guild so it appears instantly (global takes ~1h).
func diveCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        "dive",
		Description: "Deep loop on a task or research question — goal, iterate, self-review",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "task",
			Description: "What to dive on (a build task, a research question, a mockup)",
			Required:    true,
		}},
	}
}

func (b *Bot) registerDive(s *discordgo.Session, guildID string) {
	// ApplicationCommandCreate is idempotent by name, so re-registering is safe.
	if _, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, diveCommand()); err != nil {
		log.Printf("register /dive in %s: %v", guildID, err)
	}
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	for _, g := range r.Guilds {
		b.registerDive(s, g.ID)
	}
}

// onGuildCreate registers /dive in guilds the bot joins AFTER startup (onReady
// only covers guilds present at connect) — and re-covers the initial ones as
// they become available.
func (b *Bot) onGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	b.registerDive(s, g.ID)
}

func interactionUser(i *discordgo.InteractionCreate) (name, id string) {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username, i.Member.User.ID
	}
	if i.User != nil {
		return i.User.Username, i.User.ID
	}
	return "someone", ""
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.ApplicationCommandData().Name != "dive" {
		return
	}
	task := i.ApplicationCommandData().Options[0].StringValue()
	author, authorID := interactionUser(i)
	// dives run long — defer now, follow up when the loop lands
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	go func() {
		b.locks <- struct{}{}
		defer func() { <-b.locks }()
		reply := b.agent.Dive(i.ChannelID, authorID, author, task)
		chunks := splitMessage("🌀 **dive**: "+task+"\n\n"+reply.Text, 1990)
		for n, chunk := range chunks {
			params := &discordgo.WebhookParams{Content: chunk}
			if n == len(chunks)-1 {
				for _, p := range reply.Artifacts {
					f, err := os.Open(p)
					if err != nil {
						continue
					}
					defer f.Close()
					params.Files = append(params.Files, &discordgo.File{Name: artifactName(p), Reader: f})
				}
			}
			if _, err := s.FollowupMessageCreate(i.Interaction, true, params); err != nil {
				log.Printf("dive followup: %v", err)
			}
		}
	}()
}

func (b *Bot) Start() error { return b.session.Open() }
func (b *Bot) Close()       { _ = b.session.Close() }

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	content := strings.TrimSpace(m.Content)
	mentioned := false
	for _, u := range m.Mentions {
		if u.ID == s.State.User.ID {
			mentioned = true
		}
	}
	isDM := m.GuildID == ""
	if !mentioned && !isDM && !b.cfg.FocusChannels[m.ChannelID] {
		return
	}
	// strip the mention so the model sees the question, not the ping
	content = strings.ReplaceAll(content, "<@"+s.State.User.ID+">", "")
	content = strings.ReplaceAll(content, "<@!"+s.State.User.ID+">", "")
	content = strings.TrimSpace(content)

	// Pick up any image attachments for the vision pass — from THIS message and,
	// when it's a reply, from the message it replies to (so "@vela what's this?"
	// as a reply to a card photo works, not just photo-in-the-same-message).
	var images []string
	collect := func(atts []*discordgo.MessageAttachment) {
		for _, at := range atts {
			if strings.HasPrefix(at.ContentType, "image/") || isImageName(at.Filename) {
				images = append(images, at.URL)
			}
		}
	}
	collect(m.Attachments)
	if m.ReferencedMessage != nil {
		collect(m.ReferencedMessage.Attachments)
	}
	if content == "" {
		if len(images) > 0 {
			content = "what's in this image?"
		} else {
			content = "hello"
		}
	}

	if atomic.LoadInt32(&b.pending) >= maxPending {
		_ = s.MessageReactionAdd(m.ChannelID, m.ID, "🚫") // overloaded — don't pile up
		return
	}
	atomic.AddInt32(&b.pending, 1)
	go func() {
		defer atomic.AddInt32(&b.pending, -1)
		// One turn at a time by default (RAM-safe on the Nano). If she's busy,
		// queue this one — and drop an ⏳ on the message so they know they're in
		// line rather than being ignored.
		queued := false
		select {
		case b.locks <- struct{}{}:
		default:
			queued = true
			_ = s.MessageReactionAdd(m.ChannelID, m.ID, "⏳")
			b.locks <- struct{}{} // wait our turn in the queue
		}
		defer func() { <-b.locks }()
		if queued {
			_ = s.MessageReactionRemove(m.ChannelID, m.ID, "⏳", s.State.User.ID)
		}
		stop := make(chan struct{})
		go func() { // keep the typing indicator alive through long tool runs
			for {
				_ = s.ChannelTyping(m.ChannelID)
				select {
				case <-stop:
					return
				case <-time.After(8 * time.Second):
				}
			}
		}()
		reply := b.agent.Handle(m.ChannelID, m.Author.ID, m.Author.Username, content, images...)
		close(stop)
		b.send(m.ChannelID, m.Reference(), reply)
	}()
}

func (b *Bot) send(channelID string, ref *discordgo.MessageReference, r Reply) {
	text := r.Text
	if text == "" {
		text = "(no reply)"
	}
	chunks := splitMessage(text, 1990)
	for i, chunk := range chunks {
		msg := &discordgo.MessageSend{Content: chunk}
		if i == 0 {
			msg.Reference = ref
		}
		if i == len(chunks)-1 { // artifacts + confirm buttons ride the last chunk
			for _, p := range r.Artifacts {
				f, err := os.Open(p)
				if err != nil {
					continue
				}
				defer f.Close()
				msg.Files = append(msg.Files, &discordgo.File{Name: artifactName(p), Reader: f})
			}
		}
		if _, err := b.session.ChannelMessageSendComplex(channelID, msg); err != nil {
			log.Printf("send error: %v", err)
		}
	}
}

// artifactName strips the timestamp prefix for a friendlier filename.
func artifactName(p string) string {
	base := filepath.Base(p)
	if _, rest, ok := strings.Cut(base, "-"); ok && rest != "" {
		return rest
	}
	return base
}

// splitMessage breaks text at line boundaries under the Discord cap.
func splitMessage(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := strings.LastIndex(s[:max], "\n")
		if cut < max/2 {
			cut = max
			for cut > 0 && !utf8.RuneStart(s[cut]) { // don't split a multi-byte rune
				cut--
			}
		}
		out = append(out, strings.TrimRight(s[:cut], "\n"))
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

