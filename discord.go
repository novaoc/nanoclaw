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
	// ordinary turn semaphore (cap = cfg.Concurrency, default 1) — chat,
	// research, grok. Builds do NOT take a slot here; they use build instead.
	locks chan struct{}
	// build is the single coder/build lane (capacity one). A long application
	// build holds this so ordinary turns keep moving. /request refuses while
	// it is occupied.
	build    *buildLane
	threads  *OwnedThreads // forum posts Vela created; replies are addressed to her
	pending  int32         // in-flight + queued turns, to cap a spam pile-up (maxPending)
	draining int32         // set on SIGTERM: finish in-flight turns, take no new ones
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
	b := &Bot{
		cfg: cfg, agent: agent, session: s,
		locks: make(chan struct{}, cfg.Concurrency), build: newBuildLane(),
		threads: NewOwnedThreads(cfg),
	}
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onGuildCreate)
	s.AddHandler(b.onInteraction)
	return b, nil
}

// Slash commands — /dive (the deep loop), /reset (restart a channel's
// context), and /grok (the OAuth login can't work as a chat message).
// Everything else is reachable by just asking Vela.
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
			Name:        "request",
			Description: "Open a request (build something, make a video, …) as a post in the requests forum",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "goal",
				Description: "What you want made — a site, a tool, an image/video, a plan",
				Required:    true,
			}},
		},
		{
			Name:        "reset",
			Description: "Restart Vela's context in this channel (long-term memory is kept)",
		},
		{
			Name:        "grok",
			Description: "Connect a SuperGrok / X Premium sub for image+video gen (admins only)",
			Options: []*discordgo.ApplicationCommandOption{{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "action",
				Description: "login (get a link to approve), status, or logout",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "login", Value: "login"},
					{Name: "status", Value: "status"},
					{Name: "logout", Value: "logout"},
				},
			}},
		},
	}
}

func (b *Bot) registerCommands(s *discordgo.Session, guildID string) {
	// BulkOverwrite replaces the guild's whole command set, so commands removed
	// from appCommands (and strays registered by older builds) actually
	// disappear — per-name Create would leave them lingering forever.
	if _, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, guildID, appCommands()); err != nil {
		log.Printf("register commands in %s: %v", guildID, err)
	}
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	// Vela's commands are guild-scoped, so any GLOBAL command on this
	// application is a squatter left behind by other software that once
	// used the same bot token. Clear the global set so stray commands
	// don't haunt the command picker.
	if _, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, "", nil); err != nil {
		log.Printf("clear global commands: %v", err)
	}
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

// onGrokCommand drives the xAI device-code OAuth login so Vela can use a
// SuperGrok / X Premium subscription for image+video gen. Admin-gated (it
// spends the subscriber's quota). login shows a link to approve, then polls in
// the background and follows up when connected.
func (b *Bot) onGrokCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_, uid := interactionUser(i)
	if !b.cfg.Coders[uid] {
		ephemeral(s, i, "That's admin-only (the coder allowlist) — it links a paid subscription.")
		return
	}
	switch i.ApplicationCommandData().Options[0].StringValue() {
	case "logout":
		b.cfg.Grok.Clear()
		ephemeral(s, i, "Disconnected the Grok subscription. Image/video gen is off until you `/grok login` again (or an XAI_API_KEY is set).")
	case "status":
		if b.cfg.Grok.Connected() {
			ephemeral(s, i, "✅ Connected to a Grok subscription — image & video generation are live.")
		} else if b.cfg.XAIKey != "" {
			ephemeral(s, i, "Using an XAI_API_KEY (pay-as-you-go). `/grok login` to use a subscription instead.")
		} else {
			ephemeral(s, i, "Not connected. `/grok login` to link your SuperGrok / X Premium sub.")
		}
	default: // login
		dc, err := b.cfg.Grok.StartDevice()
		if err != nil {
			ephemeral(s, i, "Couldn't start the Grok login: "+err.Error())
			return
		}
		link := dc.VerificationURIComplete
		msg := "🔗 **Connect your Grok subscription**\nOpen this and approve"
		if link == "" { // no prefilled link — show the URL + code to type
			link = dc.VerificationURI
			msg += fmt.Sprintf(" — enter the code **%s**", dc.UserCode)
		}
		msg += ":\n" + link + "\n\nI'll confirm here once you've approved (the link expires in a few minutes)."
		ephemeral(s, i, msg)
		go func() {
			perr := b.cfg.Grok.PollForToken(dc)
			out := "✅ Connected! Image & video generation are live — ask me to make a picture or a clip."
			if perr != nil {
				out = "⚠️ Grok login didn't complete: " + perr.Error()
			}
			_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
				Content: out, Flags: discordgo.MessageFlagsEphemeral,
			})
		}()
	}
}

// onRequestCommand handles /request: frame the ask as a concrete goal (one
// cheap model call), open a post in the requests forum, and point the
// requester at the thread to continue the work there.
//
// While the build lane is occupied the command refuses immediately (ephemeral)
// with what is building and how much of the tracked deadline remains — not
// queued invisibly. Ordinary chat is unaffected; only /request is blocked.
func (b *Bot) onRequestCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	task := strings.TrimSpace(i.ApplicationCommandData().Options[0].StringValue())
	author, authorID := interactionUser(i)
	if task == "" {
		ephemeral(s, i, "Tell me what you want made — `/request goal: a landing page for …`")
		return
	}
	if i.GuildID == "" {
		ephemeral(s, i, "Requests need the server (the post goes in the requests forum) — run it there, not in a DM.")
		return
	}
	if name, rem, ok := b.build.busy(); ok {
		ephemeral(s, i, requestBusyMessage(name, rem))
		return
	}
	fid, fname, _, err := b.ResolveChannel(i.GuildID, b.cfg.RequestsForum)
	if err != nil {
		ephemeral(s, i, "I couldn't find a requests forum in this server — an admin needs to create a forum channel named \""+b.cfg.RequestsForum+"\" (or set NANOCLAW_REQUESTS_FORUM).")
		return
	}
	// Framing takes a model call — defer so the interaction doesn't time out.
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	go func() {
		title, body := b.agent.FrameRequest(author, task)
		body += "\n\n_requested by <@" + authorID + ">_"
		_, url, err := b.CreateForumPost(fid, title, body, ForumOrigin{
			Author: author, Request: task,
		})
		if err != nil {
			_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
				Content: "Couldn't open the post in #" + fname + " (is it a forum channel I can post in?): " + err.Error(),
			})
			return
		}
		log.Printf("request by=%s forum=%s title=%.60q", authorID, fname, title)
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: "📌 Opened your request in #" + fname + ": **" + title + "**\n" + url + "\n\nContinue there — @mention me in the thread with details or next steps and I'll keep working it.",
		})
	}()
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
	switch i.ApplicationCommandData().Name {
	case "grok":
		b.onGrokCommand(s, i)
	case "dive":
		b.onDiveCommand(s, i)
	case "request":
		b.onRequestCommand(s, i)
	case "reset":
		// Per-channel conversational state only — MEMORY.md, impressions, and
		// learned expressions survive. Open to everyone: it's the "she's gone
		// off the rails, start over" button, and it can't destroy anything.
		b.agent.ResetChannel(i.ChannelID)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "🧠 Fresh start — I've let go of this channel's conversation. (Long-term memory intact.)"},
		})
	}
}

// onDiveCommand runs the deep loop: bigger tool budget + self-review passes.
// Dives take a lane (build lane for coders, ordinary otherwise) so they respect
// the same OOM guard — unbounded /dive spam would pile up like a message flood.
func (b *Bot) onDiveCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	task := i.ApplicationCommandData().Options[0].StringValue()
	author, authorID := interactionUser(i)
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
		release, _ := b.takeLane(authorID, task, b.cfg.DiveToolIters, b.cfg.DivePasses)
		defer release()
		chID := i.ChannelID
		reply := b.agent.DiveTurn(Turn{
			ChannelID: chID, GuildID: i.GuildID, AuthorID: authorID, Author: author,
			Notify: func(text string) {
				if _, err := b.PostMessage(chID, text); err != nil {
					log.Printf("mid-turn notify: %v", err)
				}
			},
			SetBuildName: b.build.setName,
		}, task)
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

// takeLane reserves either the build lane (coder turns) or an ordinary slot.
// Release is always safe to defer — including across panics — so a failed
// build cannot wedge the lane and disable /request forever.
// queued is true when the caller had to wait for a free slot.
func (b *Bot) takeLane(authorID, hint string, toolIters, passes int) (release func(), queued bool) {
	coder := b.cfg.Coders[authorID]
	dl := turnDeadline(toolIters, passes, coder)
	if coder {
		if !b.build.tryAcquire(hint, dl) {
			queued = true
			b.build.acquire(hint, dl)
		}
		return b.build.release, queued
	}
	select {
	case b.locks <- struct{}{}:
	default:
		queued = true
		b.locks <- struct{}{}
	}
	return func() { <-b.locks }, queued
}

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
	// strip the mention so the model sees the question, not the ping
	content = strings.ReplaceAll(content, "<@"+s.State.User.ID+">", "")
	content = strings.ReplaceAll(content, "<@!"+s.State.User.ID+">", "")
	content = strings.TrimSpace(content)

	isDM := m.GuildID == ""
	ownedThread := b.threads.Has(m.ChannelID)
	ambient := false
	if !mentioned && !isDM && !b.cfg.FocusChannels[m.ChannelID] && !ownedThread {
		// Not addressed to her — the social layer records it and decides
		// (locally, no API) whether she chimes in. Almost always: no.
		if content == "" || !b.agent.Observe(m.ChannelID, m.Author.ID, m.Author.Username, content, true) {
			return
		}
		ambient = true
	} else if !isDM && content != "" {
		// She'll answer anyway — record for the transcript/impressions only.
		b.agent.Observe(m.ChannelID, m.Author.ID, m.Author.Username, content, false)
	}

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
		// Ordinary turns share the concurrency-limited lane (default 1,
		// RAM-safe on the Nano). Coder/build turns take the separate build
		// lane so a long scaffold does not freeze chat. If the chosen lane is
		// busy, queue — and drop an ⏳ so they know they're in line.
		iters, passes := b.cfg.MaxToolIters, b.cfg.Passes
		if ambient {
			iters, passes = 6, 1
		}
		release, queued := b.takeLane(m.Author.ID, content, iters, passes)
		if queued {
			_ = s.MessageReactionAdd(m.ChannelID, m.ID, "⏳")
		}
		defer release()
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
		chID := m.ChannelID
		reply := b.agent.HandleTurn(Turn{
			ChannelID: chID, GuildID: m.GuildID,
			AuthorID: m.Author.ID, Author: m.Author.Username, ImageURLs: images,
			Ambient: ambient,
			Notify: func(text string) {
				if _, err := b.PostMessage(chID, text); err != nil {
					log.Printf("mid-turn notify: %v", err)
				}
			},
			SetBuildName: b.build.setName,
		}, content)
		close(stop)
		if ambient {
			if reply.Text == "" && len(reply.Artifacts) == 0 {
				return // she chose silence — the only polite ambient failure mode
			}
			b.send(m.ChannelID, nil, reply) // joining the flow, not quote-replying
			return
		}
		b.send(m.ChannelID, m.Reference(), reply)
	}()
}

func (b *Bot) send(channelID string, ref *discordgo.MessageReference, r Reply) {
	text := r.Text
	if text == "" {
		text = "(no reply)"
	}
	// Human pacing: a short conversational
	// reply with several sentences goes out as a few separate messages with a
	// typing beat between them — a person texting, not a bot filing a report.
	// Anything with attachments, code, or real length uses the plain path.
	if len(r.Artifacts) == 0 {
		if hc := humanChunks(text); hc != nil {
			for i, chunk := range hc {
				if i > 0 {
					_ = b.session.ChannelTyping(channelID)
					time.Sleep(typeDelay(chunk))
				}
				msg := &discordgo.MessageSend{Content: chunk}
				if i == 0 {
					msg.Reference = ref
				}
				if _, err := b.session.ChannelMessageSendComplex(channelID, msg); err != nil {
					log.Printf("send error: %v", err)
				}
			}
			return
		}
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

// humanChunks splits a short conversational reply into 2-3 texting-sized
// messages at sentence/line boundaries. Returns nil when the reply should go
// out as one message (code, length, or just one sentence).
func humanChunks(text string) []string {
	if len(text) > 420 || strings.Contains(text, "```") || strings.Contains(text, "http") {
		return nil
	}
	var sents []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			sents = append(sents, splitSentences(line)...)
		}
	}
	if len(sents) < 2 {
		return nil
	}
	// pack sentences into at most 3 chunks, breaking at natural pauses
	perChunk := (len(sents) + 2) / 3
	var out []string
	for len(sents) > 0 {
		n := perChunk
		if n > len(sents) {
			n = len(sents)
		}
		out = append(out, strings.Join(sents[:n], " "))
		sents = sents[n:]
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// splitSentences cuts on sentence enders, keeping the punctuation.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if (r == '.' || r == '!' || r == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			if frag := strings.TrimSpace(s[start : i+1]); frag != "" {
				out = append(out, frag)
			}
			start = i + 1
		}
	}
	if frag := strings.TrimSpace(s[start:]); frag != "" {
		out = append(out, frag)
	}
	return out
}

// typeDelay fakes typing time for a chunk: a beat plus a bit per character,
// capped so nothing feels laggy.
func typeDelay(chunk string) time.Duration {
	d := 600*time.Millisecond + time.Duration(len(chunk))*12*time.Millisecond
	if d > 2500*time.Millisecond {
		d = 2500 * time.Millisecond
	}
	return d
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
