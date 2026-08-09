package main

import (
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// clawvault's OWN Discord app. This is what makes the split real rather than
// theater: the bk_ key is entered here (never in nanoclaw), and the Confirm
// button for a fund-moving action is posted and verified here, with the
// vault's own credentials. nanoclaw can ask the vault (over the socket) to
// queue a write; only THIS process shows the button and executes on the
// requester's click.

type VaultBot struct {
	vault   *Vault
	session *discordgo.Session
	guilds  []string
}

func NewVaultBot(vault *Vault, token string, guilds []string) (*VaultBot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages
	b := &VaultBot{vault: vault, session: s, guilds: guilds}
	s.AddHandler(b.onReady)
	s.AddHandler(b.onInteraction)
	return b, nil
}

func (b *VaultBot) Start() error { return b.session.Open() }
func (b *VaultBot) Close()       { _ = b.session.Close() }

func (b *VaultBot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	cmds := []*discordgo.ApplicationCommand{
		{
			Name:        "connect",
			Description: "Privately connect your Bankr wallet (only you can see this)",
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: "api_key",
				Description: "Your Bankr API key (bk_…) from bankr.bot/api", Required: true,
			}},
		},
		{Name: "disconnect", Description: "Remove your connected wallet"},
	}
	guilds := b.guilds
	if len(guilds) == 0 {
		for _, g := range r.Guilds {
			guilds = append(guilds, g.ID)
		}
	}
	for _, g := range guilds {
		for _, c := range cmds {
			if _, err := s.ApplicationCommandCreate(s.State.User.ID, g, c); err != nil {
				log.Printf("register /%s in %s: %v", c.Name, g, err)
			}
		}
	}
	log.Printf("clawvault discord up as %s", s.State.User.Username)
}

func user(i *discordgo.InteractionCreate) (name, id string) {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username, i.Member.User.ID
	}
	if i.User != nil {
		return i.User.Username, i.User.ID
	}
	return "someone", ""
}

func ephemeral(s *discordgo.Session, i *discordgo.InteractionCreate, text string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	})
}

func (b *VaultBot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.onCommand(s, i)
	case discordgo.InteractionMessageComponent:
		b.onButton(s, i)
	}
}

func (b *VaultBot) onCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_, uid := user(i)
	switch i.ApplicationCommandData().Name {
	case "disconnect":
		if b.vault.Disconnect(uid) {
			ephemeral(s, i, "Disconnected — your encrypted key is deleted.")
		} else {
			ephemeral(s, i, "You don't have a wallet connected.")
		}
	case "connect":
		key := strings.TrimSpace(i.ApplicationCommandData().Options[0].StringValue())
		if !ValidBankrKey(key) {
			ephemeral(s, i, "That doesn't look like a Bankr key (expected `bk_…`).")
			return
		}
		if _, err := b.vault.bankr.Prompt(key, "what is my wallet address?"); err != nil {
			ephemeral(s, i, "That key didn't work against Bankr ("+err.Error()+").")
			return
		}
		if err := b.vault.Connect(uid, key); err != nil {
			ephemeral(s, i, "Couldn't save your key: "+err.Error())
			return
		}
		ephemeral(s, i, "✅ Connected. Your key lives only in the vault, encrypted, never in the assistant. "+
			"Ask Vela for balances or a trade; I'll show you a Confirm button here for anything that moves funds.")
	}
}

// PostConfirm is called (from the socket path via the bot) to show a Confirm
// button for a queued write. Owned entirely by the vault process.
func (b *VaultBot) PostConfirm(channel, uid, token, prompt string) {
	msg := "<@" + uid + "> confirm this wallet action — **irreversible**:\n> " + prompt
	_, err := b.session.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Content: msg,
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Confirm", Style: discordgo.SuccessButton, CustomID: "cv:confirm:" + token},
			discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: "cv:cancel:" + token},
		}}},
	})
	if err != nil {
		log.Printf("post confirm: %v", err)
	}
}

func (b *VaultBot) onButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	_, uid := user(i)
	var token string
	var confirm bool
	if t, ok := strings.CutPrefix(id, "cv:confirm:"); ok {
		token, confirm = t, true
	} else if t, ok := strings.CutPrefix(id, "cv:cancel:"); ok {
		token = t
	} else {
		return
	}
	if !confirm {
		msg := "Cancelled — nothing moved."
		if err := b.vault.Cancel(token, uid); err != nil {
			msg = "⚠️ " + err.Error()
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{Content: msg, Components: []discordgo.MessageComponent{}},
		})
		return
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: "⏳ Executing…", Components: []discordgo.MessageComponent{}},
	})
	go func() {
		out, err := b.vault.Execute(token, uid)
		res := out
		if err != nil {
			res = "⚠️ " + err.Error()
		}
		if res == "" {
			res = "Done."
		}
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: res})
	}()
}
