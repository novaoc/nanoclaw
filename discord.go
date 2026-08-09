package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	cfg     *Config
	agent   *Agent
	session *discordgo.Session
	// serialize turns per channel so parallel questions don't interleave
	locks chan struct{}
}

func NewBot(cfg *Config, agent *Agent) (*Bot, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent |
		discordgo.IntentsDirectMessages
	b := &Bot{cfg: cfg, agent: agent, session: s, locks: make(chan struct{}, 2)}
	s.AddHandler(b.onMessage)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onInteraction)
	return b, nil
}

// /dive — the deep-loop skill: clear goal, bigger tool budget, self-review
// passes. Registered per guild so it appears instantly (global takes ~1h).
func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	cmds := []*discordgo.ApplicationCommand{{
		Name:        "dive",
		Description: "Deep loop on a task or research question — goal, iterate, self-review",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "task",
			Description: "What to dive on (a build task, a research question, a mockup)",
			Required:    true,
		}},
	}}
	// Wallet commands only exist when key custody is enabled (NANOCLAW_SECRET).
	if b.agent.keys.Usable() {
		cmds = append(cmds,
			&discordgo.ApplicationCommand{
				Name:        "connect",
				Description: "Privately connect your own Bankr wallet so Vela can trade for you",
				Options: []*discordgo.ApplicationCommandOption{{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "api_key",
					Description: "Your Bankr API key (bk_…) from bankr.bot/api — only you can see this",
					Required:    true,
				}},
			},
			&discordgo.ApplicationCommand{
				Name:        "disconnect",
				Description: "Remove your connected Bankr wallet — deletes your encrypted key",
			},
		)
	}
	for _, g := range r.Guilds {
		for _, cmd := range cmds {
			if _, err := s.ApplicationCommandCreate(s.State.User.ID, g.ID, cmd); err != nil {
				log.Printf("register /%s in %s: %v", cmd.Name, g.ID, err)
			}
		}
	}
}

// ephemeral replies to a slash command so only the invoker sees it.
func ephemeral(s *discordgo.Session, i *discordgo.InteractionCreate, text string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	})
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

// confirmButtons builds the Confirm/Cancel row for a pending fund-moving action.
func confirmButtons(token string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Confirm", Style: discordgo.SuccessButton, CustomID: "bankr:confirm:" + token},
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: "bankr:cancel:" + token},
	}}}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type == discordgo.InteractionMessageComponent {
		b.onButton(s, i)
		return
	}
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	name := i.ApplicationCommandData().Name
	if name == "connect" || name == "disconnect" {
		b.onWalletCommand(s, i, name)
		return
	}
	if name != "dive" {
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
				if reply.Pending != nil {
					params.Components = confirmButtons(reply.Pending.Token)
				}
			}
			if _, err := s.FollowupMessageCreate(i.Interaction, true, params); err != nil {
				log.Printf("dive followup: %v", err)
			}
		}
	}()
}

// onWalletCommand handles /connect and /disconnect. Replies are ALWAYS
// ephemeral — only the invoker ever sees them — and the key never appears
// in any bot output. Best-effort: also delete nothing to echo back.
func (b *Bot) onWalletCommand(s *discordgo.Session, i *discordgo.InteractionCreate, name string) {
	_, uid := interactionUser(i)
	if uid == "" {
		ephemeral(s, i, "Couldn't identify you — try again.")
		return
	}
	if name == "disconnect" {
		if b.agent.keys.Delete(uid) {
			ephemeral(s, i, "Disconnected — your encrypted key is deleted. I can't act on your wallet anymore.")
		} else {
			ephemeral(s, i, "You don't have a wallet connected.")
		}
		return
	}
	// connect
	key := strings.TrimSpace(i.ApplicationCommandData().Options[0].StringValue())
	if !ValidBankrKey(key) {
		ephemeral(s, i, "That doesn't look like a Bankr key (expected `bk_…`). Get one at https://bankr.bot/api with wallet access enabled.")
		return
	}
	// validate it actually works (a cheap read) before storing
	if _, err := b.agent.bankr.Prompt(key, "what is my wallet address?"); err != nil {
		ephemeral(s, i, "That key didn't work against Bankr ("+err.Error()+"). Double-check it has wallet access.")
		return
	}
	if err := b.agent.keys.Put(uid, key); err != nil {
		ephemeral(s, i, "Couldn't save your key: "+err.Error())
		return
	}
	ephemeral(s, i, "✅ Connected. Your key is encrypted at rest — the stored file is useless without the server secret, which never lives on the data card. "+
		"I act only on your wallet, only for you. Ask for balances, a token launch, or a trade. `/disconnect` wipes it anytime.")
}

// onButton handles the Confirm/Cancel buttons on a queued fund-moving action.
// The click, by the original requester, is what authorizes the transaction —
// verified in Go, so nothing the model (or a fetched web page) does can
// trigger it. Runs in the background: Bankr can take up to ~90s.
func (b *Bot) onButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	_, uid := interactionUser(i)
	var action, token string
	if t, ok := strings.CutPrefix(id, "bankr:confirm:"); ok {
		action, token = "confirm", t
	} else if t, ok := strings.CutPrefix(id, "bankr:cancel:"); ok {
		action, token = "cancel", t
	} else {
		return
	}

	if action == "cancel" {
		msg := "Cancelled — nothing moved."
		if err := b.agent.CancelBankr(token, uid); err != nil {
			msg = "⚠️ " + err.Error()
		}
		// replace the buttons with the outcome (edit, keep it ephemeral-ish)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{Content: msg, Components: []discordgo.MessageComponent{}},
		})
		return
	}

	// confirm: acknowledge (drop the buttons) then execute in the background
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: "⏳ Executing…", Components: []discordgo.MessageComponent{}},
	})
	go func() {
		b.locks <- struct{}{}
		defer func() { <-b.locks }()
		out, err := b.agent.ConfirmBankr(token, uid)
		result := out
		if err != nil {
			result = "⚠️ " + err.Error()
		}
		if result == "" {
			result = "Done."
		}
		for n, chunk := range splitMessage(result, 1990) {
			_, e := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: chunk})
			if e != nil {
				log.Printf("confirm followup: %v", e)
			}
			_ = n
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
	if content == "" {
		content = "hello"
	}

	go func() {
		b.locks <- struct{}{} // cap concurrent turns (256 MB of RAM, be humble)
		defer func() { <-b.locks }()
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
		reply := b.agent.Handle(m.ChannelID, m.Author.ID, m.Author.Username, content)
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
			if r.Pending != nil {
				msg.Components = confirmButtons(r.Pending.Token)
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
		}
		out = append(out, strings.TrimRight(s[:cut], "\n"))
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

