// gwprobe empirically answers, on Vela's own board with her own token and
// intent set, the two questions the watchdog design depends on:
//
//  1. does a func(*Session, interface{}) catch-all see READY and friends?
//  2. which RequestGuildMembers form does the gateway actually answer
//     WITHOUT the privileged GuildMembers intent — user_ids, empty query,
//     or a prefix query?
//
// It opens a second session alongside the live bot (Discord allows
// concurrent sessions per token), watches events for a bit, fires the three
// probe forms, and reports. Read-only; sends nothing to any channel.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func envToken(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		for _, key := range []string{"VELA_DISCORD_TOKEN=", "DISCORD_TOKEN=", "NANOCLAW_DISCORD_TOKEN="} {
			if v, ok := strings.CutPrefix(line, key); ok && v != "" {
				return strings.Trim(v, `"'`)
			}
		}
	}
	return ""
}

func main() {
	token := envToken("/etc/nanoclaw.env")
	if token == "" {
		fmt.Println("FATAL: no discord token in /etc/nanoclaw.env")
		os.Exit(1)
	}

	s, err := discordgo.New("Bot " + token)
	if err != nil {
		fmt.Println("FATAL:", err)
		os.Exit(1)
	}
	// Exactly the live bot's intents — the whole point is matching its view.
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent | discordgo.IntentsDirectMessages

	events := make(chan string, 256)
	s.AddHandler(func(_ *discordgo.Session, e interface{}) {
		select {
		case events <- fmt.Sprintf("%T", e):
		default:
		}
	})

	if err := s.Open(); err != nil {
		fmt.Println("FATAL open:", err)
		os.Exit(1)
	}
	defer s.Close()

	guildID := ""
	settle := time.After(12 * time.Second)
settled:
	for {
		select {
		case t := <-events:
			fmt.Println("event:", t)
		case <-settle:
			break settled
		}
	}
	s.State.RLock()
	self := ""
	if s.State.User != nil {
		self = s.State.User.ID
	}
	if len(s.State.Guilds) > 0 {
		guildID = s.State.Guilds[0].ID
	}
	s.State.RUnlock()
	fmt.Printf("settled: self=%s guild=%s\n", self, guildID)
	if guildID == "" || self == "" {
		fmt.Println("FATAL: no guild/self after settle — catch-all handler saw nothing?")
		os.Exit(1)
	}

	runProbe := func(name string, send func() error) {
		if err := send(); err != nil {
			fmt.Printf("%s: send error: %v\n", name, err)
			return
		}
		deadline := time.After(15 * time.Second)
		for {
			select {
			case t := <-events:
				fmt.Println("event:", t)
				if t == "*discordgo.GuildMembersChunk" {
					fmt.Printf("%s: ANSWERED\n", name)
					return
				}
			case <-deadline:
				fmt.Printf("%s: TIMEOUT\n", name)
				return
			}
		}
	}

	runProbe("probe-user-ids", func() error {
		return s.RequestGuildMembersList(guildID, []string{self}, 1, "p1", false)
	})
	runProbe("probe-empty-query", func() error {
		return s.RequestGuildMembers(guildID, "", 1, "p2", false)
	})
	runProbe("probe-prefix-query", func() error {
		return s.RequestGuildMembers(guildID, "a", 1, "p3", false)
	})
	fmt.Println("done")
}
