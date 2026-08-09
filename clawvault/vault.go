package main

import "strings"

// Vault is the one place keys are decrypted and used. Every path here either
// returns text or an error — NEVER a key. nanoclaw (via the socket) can reach
// only Prompt/Status/Disconnect; connect and confirmation are Discord-native
// in this process, so a compromised nanoclaw can send prompts but can't learn
// a key, skip a confirmation, or beat a cap.

type Vault struct {
	keys     *KeyStore
	bankr    *Bankr
	policy   *Policy
	confirms *Confirmations
	// onQueue posts the Confirm button via the vault's own Discord app.
	onQueue func(channel, uid, token, prompt string)
}

func NewVault(ks *KeyStore, b *Bankr, p *Policy, c *Confirmations) *Vault {
	return &Vault{keys: ks, bankr: b, policy: p, confirms: c, onQueue: func(_, _, _, _ string) {}}
}

func (v *Vault) Connected(uid string) bool { return v.keys.Has(uid) }

// Connect stores a user's key (Discord-native path only — see discord.go).
func (v *Vault) Connect(uid, key string) error {
	if err := v.keys.Put(uid, key); err != nil {
		return err
	}
	v.policy.Audit("connect", uid, "")
	return nil
}

func (v *Vault) Disconnect(uid string) bool {
	ok := v.keys.Delete(uid)
	if ok {
		v.policy.Audit("disconnect", uid, "")
	}
	return ok
}

// PromptResult is what the socket returns for a prompt: a read runs and comes
// back in Result; a write is queued (the vault will post its own Confirm
// button to Channel) and Queued is set — no key, ever.
type PromptResult struct {
	Result string
	Queued bool
}

func (v *Vault) Prompt(uid, channel, text string) (PromptResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return PromptResult{}, errBadRequest("empty prompt")
	}
	if !v.keys.Has(uid) {
		return PromptResult{}, errNotConnected
	}
	if IsWalletRead(text) {
		key, _ := v.keys.Get(uid)
		out, err := v.bankr.Prompt(key, text)
		if err != nil {
			return PromptResult{}, err
		}
		v.policy.Audit("read", uid, text)
		return PromptResult{Result: out}, nil
	}
	// write: policy pre-check, then queue for the user's button click
	if err := v.policy.CheckWrite(uid); err != nil {
		v.policy.Audit("write-blocked", uid, err.Error())
		return PromptResult{}, err
	}
	p := v.confirms.Add(uid, channel, text)
	v.policy.Audit("write-queued", uid, text)
	v.onQueue(p.Channel, p.UID, p.Token, p.Prompt) // vault posts its own button
	return PromptResult{Queued: true}, nil
}

// Execute runs a queued write after the vault's own Discord button is clicked
// by the requester. Re-checks policy at execution time (fail-closed).
func (v *Vault) Execute(token, clicker string) (string, error) {
	p, err := v.confirms.Take(token, clicker)
	if err != nil {
		return "", err
	}
	if err := v.policy.CheckWrite(p.UID); err != nil {
		v.policy.Audit("write-blocked", p.UID, err.Error())
		return "", err
	}
	key, ok := v.keys.Get(p.UID)
	if !ok {
		return "", errNotConnected
	}
	out, err := v.bankr.Prompt(key, p.Prompt)
	if err != nil {
		v.policy.Audit("write-failed", p.UID, err.Error())
		return "", err
	}
	v.policy.RecordWrite(p.UID)
	v.policy.Audit("write-executed", p.UID, p.Prompt)
	return out, nil
}

func (v *Vault) Cancel(token, clicker string) error {
	err := v.confirms.Cancel(token, clicker)
	if err == nil {
		v.policy.Audit("write-cancelled", clicker, token)
	}
	return err
}
