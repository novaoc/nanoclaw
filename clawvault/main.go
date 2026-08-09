// clawvault — key custody split out of the assistant. Owns NANOCLAW_SECRET
// and the keys, executes Bankr prompts, enforces policy, and runs its own
// Discord app for /connect + confirmation. Exposes only a Unix socket to
// nanoclaw, which can send prompts but can never learn a key. See CLAWVAULT.md.
package main

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	// also read /etc/clawvault.env then ./clawvault.env
	for _, p := range []string{"/etc/clawvault.env", "clawvault.env"} {
		if f, err := os.Open(p); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if kk, vv, ok := strings.Cut(line, "="); ok && strings.TrimSpace(kk) == k {
					f.Close()
					return strings.Trim(strings.TrimSpace(vv), `"`)
				}
			}
			f.Close()
		}
	}
	return def
}

func main() {
	secret := env("NANOCLAW_SECRET", "")
	if secret == "" {
		log.Fatal("NANOCLAW_SECRET is required")
	}
	token := env("VAULT_DISCORD_TOKEN", "")
	if token == "" {
		log.Fatal("VAULT_DISCORD_TOKEN is required (clawvault's own Discord app)")
	}
	dataDir := env("CLAWVAULT_DATA", "vault-data")
	sockPath := env("CLAWVAULT_SOCKET", dataDir+"/clawvault.sock")
	maxPerDay, _ := strconv.Atoi(env("VAULT_MAX_WRITES_PER_DAY", "20"))
	if maxPerDay < 1 {
		maxPerDay = 20
	}
	var guilds []string
	for _, g := range strings.Split(env("VAULT_GUILDS", ""), ",") {
		if g = strings.TrimSpace(g); g != "" {
			guilds = append(guilds, g)
		}
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	ks, err := NewKeyStore(dataDir+"/keys", secret)
	if err != nil {
		log.Fatalf("keystore: %v", err)
	}
	policy, err := NewPolicy(dataDir+"/audit.log", maxPerDay, 5*time.Second)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}
	vault := NewVault(ks, NewBankr(env("BANKR_API_URL", "")), policy, NewConfirmations())

	bot, err := NewVaultBot(vault, token, guilds)
	if err != nil {
		log.Fatalf("discord: %v", err)
	}
	vault.onQueue = bot.PostConfirm // vault posts its own Confirm buttons
	if err := bot.Start(); err != nil {
		log.Fatalf("discord connect: %v", err)
	}
	defer bot.Close()

	srv := NewServer(vault, sockPath)
	log.Printf("clawvault up — socket=%s data=%s maxWrites/day=%d", sockPath, dataDir, maxPerDay)
	if err := srv.Serve(); err != nil {
		log.Fatalf("socket: %v", err)
	}
}
