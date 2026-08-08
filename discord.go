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
	return b, nil
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
		reply := b.agent.Handle(m.ChannelID, m.Author.Username, content)
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
		if i == len(chunks)-1 { // artifacts ride the last chunk
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
		}
		out = append(out, strings.TrimRight(s[:cut], "\n"))
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

