package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Discord is the actuator the tool layer uses to take real actions in the
// server (moderation, forum posts). Kept as an interface so the agent/tools
// don't depend on discordgo directly and stay unit-testable. Bot implements it.
type Discord interface {
	Timeout(guildID, userID string, d time.Duration, reason string) error
	Kick(guildID, userID, reason string) error
	Ban(guildID, userID, reason string, deleteDays int) error
	DeleteMessage(channelID, messageID string) error
	Slowmode(channelID string, seconds int) error
	ResolveMember(guildID, query string) (id, name string, err error)
	ResolveChannel(guildID, query string) (id, name string, ctype int, err error)
	CreateForumPost(forumChannelID, title, body string) (url string, err error)
	PostMessage(channelID, body string) (url string, err error)
}

// idFromMention extracts a bare user id from "<@123>"/"<@!123>"/"123". For a
// mention it keeps only the digits; a plain id passes through unchanged.
func idFromMention(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<@") {
		var d strings.Builder
		for _, r := range s {
			if r >= '0' && r <= '9' {
				d.WriteRune(r)
			}
		}
		return d.String()
	}
	return s
}

func (b *Bot) Timeout(guildID, userID string, d time.Duration, reason string) error {
	until := time.Now().Add(d)
	return b.session.GuildMemberTimeout(guildID, userID, &until, discordgo.WithAuditLogReason(reason))
}

func (b *Bot) Kick(guildID, userID, reason string) error {
	return b.session.GuildMemberDeleteWithReason(guildID, userID, reason)
}

func (b *Bot) Ban(guildID, userID, reason string, deleteDays int) error {
	return b.session.GuildBanCreateWithReason(guildID, userID, reason, deleteDays)
}

func (b *Bot) DeleteMessage(channelID, messageID string) error {
	return b.session.ChannelMessageDelete(channelID, messageID)
}

func (b *Bot) Slowmode(channelID string, seconds int) error {
	_, err := b.session.ChannelEditComplex(channelID, &discordgo.ChannelEdit{RateLimitPerUser: &seconds})
	return err
}

// ResolveMember turns a mention, raw id, or username into (id, displayname).
func (b *Bot) ResolveMember(guildID, query string) (string, string, error) {
	if id := idFromMention(query); id != "" && !strings.ContainsAny(id, " @#") {
		if m, err := b.session.GuildMember(guildID, id); err == nil {
			return m.User.ID, m.User.Username, nil
		}
	}
	q := strings.TrimPrefix(strings.TrimSpace(query), "@")
	members, err := b.session.GuildMembersSearch(guildID, q, 5)
	if err != nil {
		return "", "", err
	}
	if len(members) == 0 {
		return "", "", fmt.Errorf("no member matching %q", query)
	}
	return members[0].User.ID, members[0].User.Username, nil
}

// ResolveChannel matches a channel by id or (case-insensitive) name within the
// guild, returning its id, name, and type.
func (b *Bot) ResolveChannel(guildID, query string) (string, string, int, error) {
	chans, err := b.session.GuildChannels(guildID)
	if err != nil {
		return "", "", 0, err
	}
	q := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(query, "#")))
	for _, c := range chans { // exact id first
		if c.ID == q {
			return c.ID, c.Name, int(c.Type), nil
		}
	}
	for _, c := range chans { // then exact name
		if strings.ToLower(c.Name) == q {
			return c.ID, c.Name, int(c.Type), nil
		}
	}
	for _, c := range chans { // then contains
		if strings.Contains(strings.ToLower(c.Name), q) {
			return c.ID, c.Name, int(c.Type), nil
		}
	}
	return "", "", 0, fmt.Errorf("no channel matching %q", query)
}

// CreateForumPost opens a new forum thread (post) with a title and first message.
func (b *Bot) CreateForumPost(forumChannelID, title, body string) (string, error) {
	th, err := b.session.ForumThreadStartComplex(forumChannelID,
		&discordgo.ThreadStart{Name: title, AutoArchiveDuration: 1440},
		&discordgo.MessageSend{Content: body})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s", th.GuildID, th.ID), nil
}

// PostMessage sends a message to a channel or thread (used to reply to a forum
// post: pass the thread id).
func (b *Bot) PostMessage(channelID, body string) (string, error) {
	m, err := b.session.ChannelMessageSend(channelID, body)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", m.GuildID, channelID, m.ID), nil
}
