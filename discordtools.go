package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Tool handlers that act on Discord (moderation, forum posts) and the
// secret-wipe tool. All gated: moderation to the mod allowlist, secrets to the
// coder allowlist.

func (tc *ToolCtx) clearSecrets() string {
	if !tc.isCoder() {
		return "REFUSED: only a coder can manage deploy secrets."
	}
	n := tc.cfg.Secrets.Clear()
	log.Printf("secrets cleared (%d) by=%s", n, tc.authorID)
	if n == 0 {
		return "No secrets were set — nothing to wipe."
	}
	return fmt.Sprintf("Wiped %d secret(s) from memory and disk. Gone.", n)
}

func (tc *ToolCtx) isMod() bool { return tc.cfg.Mods[tc.authorID] }

// parseDur reads "10m"/"1h"/"1d"/"90s" into a Duration, bounded to Discord's
// 28-day timeout ceiling.
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("no duration")
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			d := time.Duration(n) * 24 * time.Hour
			if d > 28*24*time.Hour {
				d = 28 * 24 * time.Hour
			}
			return d, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q (try 10m, 1h, 1d)", s)
	}
	if d > 28*24*time.Hour {
		d = 28 * 24 * time.Hour
	}
	return d, nil
}

func (tc *ToolCtx) moderate(a toolArgs) string {
	if tc.disc == nil || tc.guildID == "" {
		return "moderation isn't available here (needs to run in a server channel)."
	}
	if !tc.isMod() {
		return "REFUSED: moderation is limited to the mod allowlist (NANOCLAW_MODS), and this user isn't on it. Tell them plainly."
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	log.Printf("moderate action=%s by=%s user=%.40q chan=%s", action, tc.authorID, a.User, tc.channelID)

	switch action {
	case "timeout":
		id, name, err := tc.disc.ResolveMember(tc.guildID, a.User)
		if err != nil {
			return "couldn't find that member: " + err.Error()
		}
		d, err := parseDur(a.Duration)
		if err != nil {
			return "timeout needs a duration — " + err.Error()
		}
		if err := tc.disc.Timeout(tc.guildID, id, d, a.Reason); err != nil {
			return "timeout failed (do I have the Moderate Members permission?): " + err.Error()
		}
		return fmt.Sprintf("Timed out %s for %s%s.", name, a.Duration, reasonSuffix(a.Reason))
	case "kick":
		id, name, err := tc.disc.ResolveMember(tc.guildID, a.User)
		if err != nil {
			return "couldn't find that member: " + err.Error()
		}
		if err := tc.disc.Kick(tc.guildID, id, a.Reason); err != nil {
			return "kick failed (do I have the Kick Members permission?): " + err.Error()
		}
		return fmt.Sprintf("Kicked %s%s.", name, reasonSuffix(a.Reason))
	case "ban":
		id, name, err := tc.disc.ResolveMember(tc.guildID, a.User)
		if err != nil {
			return "couldn't find that member: " + err.Error()
		}
		banDays := a.Days
		if banDays < 0 || banDays > 7 {
			banDays = 0 // Discord allows 0-7 days of message deletion on ban
		}
		if err := tc.disc.Ban(tc.guildID, id, a.Reason, banDays); err != nil {
			return "ban failed (do I have the Ban Members permission?): " + err.Error()
		}
		return fmt.Sprintf("Banned %s%s.", name, reasonSuffix(a.Reason))
	case "delete":
		if a.Message == "" {
			return "delete needs the message id."
		}
		if err := tc.disc.DeleteMessage(tc.channelID, a.Message); err != nil {
			return "delete failed (do I have Manage Messages here?): " + err.Error()
		}
		return "Deleted that message."
	case "slowmode":
		secs := a.Seconds
		if secs == 0 && a.Days > 0 {
			secs = a.Days // tolerate the model passing it as days
		}
		if err := tc.disc.Slowmode(tc.channelID, secs); err != nil {
			return "slowmode failed (do I have Manage Channel here?): " + err.Error()
		}
		if secs == 0 {
			return "Slowmode off for this channel."
		}
		return fmt.Sprintf("Slowmode set to %ds per user in this channel.", secs)
	}
	return "moderate: action must be timeout, kick, ban, delete, or slowmode."
}

func reasonSuffix(r string) string {
	if strings.TrimSpace(r) == "" {
		return ""
	}
	return " — " + r
}

func (tc *ToolCtx) discordForum(a toolArgs) string {
	if tc.disc == nil || tc.guildID == "" {
		return "forum posting isn't available here (needs to run in a server)."
	}
	body := strings.TrimSpace(a.Body)
	if body == "" {
		return "forum: I need the message body to post."
	}
	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "post":
		if a.Channel == "" || strings.TrimSpace(a.Title) == "" {
			return "forum post needs a channel (the forum's name or id) and a title."
		}
		cid, cname, _, err := tc.disc.ResolveChannel(tc.guildID, a.Channel)
		if err != nil {
			return "couldn't find that forum channel: " + err.Error()
		}
		_, url, err := tc.disc.CreateForumPost(cid, a.Title, body, ForumOrigin{
			Author: tc.author, Request: tc.request,
		})
		if err != nil {
			return "couldn't create the post (is that a forum channel, and do I have permission?): " + err.Error()
		}
		log.Printf("forum post by=%s chan=%s(%s) title=%.60q", tc.authorID, cname, cid, a.Title)
		return fmt.Sprintf("Posted \"%s\" in #%s — %s", a.Title, cname, url)
	case "reply":
		if a.Thread == "" {
			return "forum reply needs the thread (its id, or the post title)."
		}
		tid := a.Thread
		// allow replying by post title: resolve a channel/thread by name
		if strings.ContainsAny(tid, " ") || !isSnowflake(tid) {
			if id, _, _, err := tc.disc.ResolveChannel(tc.guildID, a.Thread); err == nil {
				tid = id
			}
		}
		url, err := tc.disc.PostMessage(tid, body)
		if err != nil {
			return "couldn't reply to that thread: " + err.Error()
		}
		log.Printf("forum reply by=%s thread=%s", tc.authorID, tid)
		return "Replied — " + url
	}
	return "forum: action must be post or reply."
}

// isSnowflake reports whether s looks like a Discord id (all digits).
func isSnowflake(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 5 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
