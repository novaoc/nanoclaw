package main

import (
	"fmt"
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
	locks    chan struct{}
	pending  int32 // in-flight + queued turns, to cap a spam pile-up (maxPending)
	draining int32 // set on SIGTERM: finish in-flight turns, take no new ones
}

// maxPending bounds queued goroutines so a message flood can't OOM the 128 MB
// board — past this, new messages get a 🚫 react instead of piling up.
const maxPending = 24

func NewBot(cfg *Config, agent *Agent) (*Bot, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, err
	}
	// NOTE: no Server Members (GuildMembers) privileged intent — moderation
	// resolves members via REST (GuildMember/GuildMembersSearch), which needs no
	// gateway intent. Requesting a privileged intent that isn't enabled in the
	// Developer Portal would make Open() fail and crash-loop the bot offline.
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent | discordgo.IntentsDirectMessages
	b := &Bot{cfg: cfg, agent: agent, session: s, locks: make(chan struct{}, cfg.Concurrency)}
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onGuildCreate)
	s.AddHandler(b.onInteraction)
	return b, nil
}

// /dive — the deep-loop skill: clear goal, bigger tool budget, self-review
// passes. Registered per guild so it appears instantly (global takes ~1h).
func appCommands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "dive",
			Description: "Deep loop on a task or research question — goal, iterate, self-review",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "task",
				Description: "What to dive on (a build task, a research question, a mockup)",
				Required:    true,
			}},
		},
		{
			Name:        "memory",
			Description: "View or clear Vela's long-term memory (admins only)",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "action",
				Description: "view the notes, or clear them",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "view", Value: "view"},
					{Name: "clear", Value: "clear"},
				},
			}},
		},
		{
			Name:        "keys",
			Description: "Hand Vela a deploy secret privately, list, or wipe (coders only)",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "action",
				Description: "add a key (private popup), list names, or clear all",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "add", Value: "add"},
					{Name: "list", Value: "list"},
					{Name: "clear", Value: "clear"},
				},
			}},
		},
		{
			Name:        "focus",
			Description: "Toggle this channel for mention-free replies (admins only)",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "action",
				Description: "on, off, or status for this channel",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "on", Value: "on"},
					{Name: "off", Value: "off"},
					{Name: "status", Value: "status"},
				},
			}},
		},
	}
}

func (b *Bot) registerCommands(s *discordgo.Session, guildID string) {
	// ApplicationCommandCreate is idempotent by name, so re-registering is safe.
	for _, c := range appCommands() {
		if _, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, c); err != nil {
			log.Printf("register /%s in %s: %v", c.Name, guildID, err)
		}
	}
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	for _, g := range r.Guilds {
		b.registerCommands(s, g.ID)
	}
}

// onGuildCreate registers commands in guilds the bot joins AFTER startup (onReady
// only covers guilds present at connect) — needs the Guilds intent to fire.
func (b *Bot) onGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	b.registerCommands(s, g.ID)
}

func ephemeral(s *discordgo.Session, i *discordgo.InteractionCreate, text string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	})
}

// onMemoryCommand handles /memory — admin-only (coder allowlist), so poisoned
// notes are discoverable and purgeable instead of silently steering the bot.
func (b *Bot) onMemoryCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_, uid := interactionUser(i)
	if !b.cfg.Coders[uid] {
		ephemeral(s, i, "That's admin-only (the coder allowlist). Ask Aregus.")
		return
	}
	switch i.ApplicationCommandData().Options[0].StringValue() {
	case "clear":
		if err := clearMemory(b.cfg); err != nil {
			ephemeral(s, i, "Couldn't clear it: "+err.Error())
			return
		}
		ephemeral(s, i, "🧹 Long-term memory set aside (kept as MEMORY.md.bak, recoverable over SSH).")
	default: // view
		m := strings.TrimSpace(fullMemory(b.cfg))
		if m == "" {
			ephemeral(s, i, "Memory is empty.")
			return
		}
		if len(m) > 1800 { // too long to inline — attach the whole file
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Long-term memory (attached — full file):",
					Flags:   discordgo.MessageFlagsEphemeral,
					Files:   []*discordgo.File{{Name: "MEMORY.md", Reader: strings.NewReader(m)}},
				},
			})
			return
		}
		ephemeral(s, i, "```\n"+m+"\n```")
	}
}

// onKeysCommand handles /keys — coder-only. "add" opens a MODAL so the secret
// value is typed into a private popup, never into visible channel text.
func (b *Bot) onKeysCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_, uid := interactionUser(i)
	if !b.cfg.Coders[uid] {
		ephemeral(s, i, "That's coder-only (the deploy-key allowlist). Ask Aregus.")
		return
	}
	switch i.ApplicationCommandData().Options[0].StringValue() {
	case "add":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: &discordgo.InteractionResponseData{
				CustomID: "keys_add",
				Title:    "Add a deploy secret",
				Components: []discordgo.MessageComponent{
					discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
						CustomID: "name", Label: "Name (e.g. HETZNER_TOKEN)", Style: discordgo.TextInputShort,
						Placeholder: "HETZNER_TOKEN", Required: true, MaxLength: 64,
					}}},
					discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
						CustomID: "value", Label: "Value (hidden; never shown in chat)", Style: discordgo.TextInputParagraph,
						Required: true, MaxLength: 4000,
					}}},
				},
			},
		})
	case "clear":
		n := b.cfg.Secrets.Clear()
		ephemeral(s, i, fmt.Sprintf("🧹 Wiped %d secret(s) from memory and disk.", n))
	default: // list
		names := b.cfg.Secrets.Names()
		if len(names) == 0 {
			ephemeral(s, i, "No secrets set. Add one with `/keys add`.")
			return
		}
		ephemeral(s, i, "🔐 Held (values hidden): "+strings.Join(names, ", ")+"\nThey're injected into shell by name; I'll wipe them with clear_secrets when the task's done, or `/keys clear`.")
	}
}

// onModalSubmit stores a submitted secret. The value never touches the model,
// channel history, or the logs — only the confirmation (by name) is shown.
func (b *Bot) onModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.ModalSubmitData().CustomID != "keys_add" {
		return
	}
	_, uid := interactionUser(i)
	if !b.cfg.Coders[uid] {
		ephemeral(s, i, "Coder-only.")
		return
	}
	var name, value string
	for _, row := range i.ModalSubmitData().Components {
		for _, c := range row.(*discordgo.ActionsRow).Components {
			ti := c.(*discordgo.TextInput)
			switch ti.CustomID {
			case "name":
				name = ti.Value
			case "value":
				value = ti.Value
			}
		}
	}
	stored, err := b.cfg.Secrets.Set(name, value)
	if err != nil {
		ephemeral(s, i, "Couldn't store that: "+err.Error())
		return
	}
	log.Printf("secret set name=%s by=%s (value hidden)", stored, uid)
	ephemeral(s, i, fmt.Sprintf("🔐 Stored **%s** (value hidden). It's available to shell as $%s; I'll wipe it when the deploy's done or on `/keys clear`.", stored, stored))
}

// onFocusCommand toggles the current channel for mention-free replies (admins).
func (b *Bot) onFocusCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_, uid := interactionUser(i)
	if !b.cfg.Coders[uid] {
		ephemeral(s, i, "That's admin-only (the coder allowlist).")
		return
	}
	switch i.ApplicationCommandData().Options[0].StringValue() {
	case "on":
		b.cfg.FocusChannels[i.ChannelID] = true
		saveFocus(b.cfg.DataDir, b.cfg.FocusChannels)
		ephemeral(s, i, "✅ I'll now reply in this channel without needing an @mention.")
	case "off":
		delete(b.cfg.FocusChannels, i.ChannelID)
		saveFocus(b.cfg.DataDir, b.cfg.FocusChannels)
		ephemeral(s, i, "✅ Back to mention-only in this channel.")
	default:
		if b.cfg.FocusChannels[i.ChannelID] {
			ephemeral(s, i, "This channel is a focus channel — I reply here without a mention.")
		} else {
			ephemeral(s, i, "Mention-only here. `/focus on` to change that.")
		}
	}
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
	if i.Type == discordgo.InteractionModalSubmit {
		b.onModalSubmit(s, i)
		return
	}
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	switch i.ApplicationCommandData().Name {
	case "memory":
		b.onMemoryCommand(s, i)
		return
	case "keys":
		b.onKeysCommand(s, i)
		return
	case "focus":
		b.onFocusCommand(s, i)
		return
	case "dive":
	default:
		return
	}
	task := i.ApplicationCommandData().Options[0].StringValue()
	author, authorID := interactionUser(i)
	// dives queue on the same semaphore as messages, so they must respect the
	// same OOM guard — unbounded /dive spam would pile up goroutines just like
	// a message flood.
	if atomic.LoadInt32(&b.draining) != 0 {
		ephemeral(s, i, "🔌 I'm restarting for an update — try the dive again in a minute.")
		return
	}
	if atomic.LoadInt32(&b.pending) >= maxPending {
		ephemeral(s, i, "🚫 I'm at my queue limit right now — try the dive again in a minute.")
		return
	}
	atomic.AddInt32(&b.pending, 1)
	// dives run long — defer now, follow up when the loop lands
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	go func() {
		defer atomic.AddInt32(&b.pending, -1)
		b.locks <- struct{}{}
		defer func() { <-b.locks }()
		reply := b.agent.DiveTurn(Turn{ChannelID: i.ChannelID, GuildID: i.GuildID, AuthorID: authorID, Author: author}, task)
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
				// interaction tokens die after 15 min — a dive queued behind long
				// turns can outlive one. Don't swallow the finished work: post the
				// rest as a normal channel message instead.
				log.Printf("dive followup: %v — falling back to a channel message", err)
				b.send(i.ChannelID, nil, Reply{Text: strings.Join(chunks[n:], "\n"), Artifacts: reply.Artifacts})
				return
			}
		}
	}()
}

func (b *Bot) Start() error { return b.session.Open() }
func (b *Bot) Close()       { _ = b.session.Close() }

// Drain stops accepting new turns and waits (bounded) for in-flight ones to
// finish and send their replies — so a hot-deploy's killall doesn't eat a turn
// mid-flight and leave the asker staring at a typing indicator that never
// resolves. New messages during the drain are ignored, same as during the
// restart gap that follows.
func (b *Bot) Drain(timeout time.Duration) {
	atomic.StoreInt32(&b.draining, 1)
	deadline := time.Now().Add(timeout)
	for atomic.LoadInt32(&b.pending) > 0 && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
}

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || atomic.LoadInt32(&b.draining) != 0 {
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
		reply := b.agent.HandleTurn(Turn{
			ChannelID: m.ChannelID, GuildID: m.GuildID,
			AuthorID: m.Author.ID, Author: m.Author.Username, ImageURLs: images,
		}, content)
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

