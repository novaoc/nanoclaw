package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Per-user Bankr key custody. Each connected user's key is AES-GCM encrypted
// under a server secret (NANOCLAW_SECRET) and written to data/keys/<uid>.enc,
// so a stolen SD card without the secret can't decrypt anything. Vela then
// runs each person's trades against THEIR wallet — nobody spends anyone
// else's funds.

type KeyStore struct {
	dir    string
	aead   cipher.AEAD
	mu     sync.RWMutex
	cache  map[string]string // uid → plaintext key, lazily decrypted
	usable bool              // false when no NANOCLAW_SECRET is set
}

var bkKeyRe = regexp.MustCompile(`^bk_[A-Za-z0-9_-]{8,120}$`)

// ValidBankrKey — cheap format gate before we ever store or spend on it.
func ValidBankrKey(s string) bool { return bkKeyRe.MatchString(s) }

func NewKeyStore(cfg *Config) *KeyStore {
	ks := &KeyStore{
		dir:   filepath.Join(cfg.DataDir, "keys"),
		cache: map[string]string{},
	}
	if cfg.Secret == "" {
		return ks // usable=false — /connect refuses, feature stays off
	}
	sum := sha256.Sum256([]byte(cfg.Secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return ks
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return ks
	}
	_ = os.MkdirAll(ks.dir, 0o700)
	ks.aead = aead
	ks.usable = true
	return ks
}

func (ks *KeyStore) Usable() bool { return ks != nil && ks.usable }

func (ks *KeyStore) path(uid string) string {
	// uid comes from Discord (numeric snowflake); sanitize defensively
	safe := regexp.MustCompile(`[^0-9A-Za-z_-]`).ReplaceAllString(uid, "")
	return filepath.Join(ks.dir, safe+".enc")
}

func (ks *KeyStore) Put(uid, key string) error {
	if !ks.usable {
		return errors.New("key storage is not enabled (NANOCLAW_SECRET unset)")
	}
	if !ValidBankrKey(key) {
		return errors.New("that doesn't look like a Bankr key (expected bk_…)")
	}
	nonce := make([]byte, ks.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := ks.aead.Seal(nonce, nonce, []byte(key), []byte(uid)) // uid as AAD
	enc := base64.StdEncoding.EncodeToString(sealed)
	if err := os.WriteFile(ks.path(uid), []byte(enc), 0o600); err != nil {
		return err
	}
	ks.mu.Lock()
	ks.cache[uid] = key
	ks.mu.Unlock()
	return nil
}

func (ks *KeyStore) Get(uid string) (string, bool) {
	if !ks.usable {
		return "", false
	}
	ks.mu.RLock()
	if k, ok := ks.cache[uid]; ok {
		ks.mu.RUnlock()
		return k, true
	}
	ks.mu.RUnlock()
	raw, err := os.ReadFile(ks.path(uid))
	if err != nil {
		return "", false
	}
	sealed, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil || len(sealed) < ks.aead.NonceSize() {
		return "", false
	}
	nonce, ct := sealed[:ks.aead.NonceSize()], sealed[ks.aead.NonceSize():]
	pt, err := ks.aead.Open(nil, nonce, ct, []byte(uid))
	if err != nil {
		return "", false // wrong secret, tampered file, or uid mismatch
	}
	key := string(pt)
	ks.mu.Lock()
	ks.cache[uid] = key
	ks.mu.Unlock()
	return key, true
}

func (ks *KeyStore) Has(uid string) bool {
	_, ok := ks.Get(uid)
	return ok
}

func (ks *KeyStore) Delete(uid string) bool {
	if ks == nil || !ks.usable {
		return false
	}
	ks.mu.Lock()
	delete(ks.cache, uid)
	ks.mu.Unlock()
	return os.Remove(ks.path(uid)) == nil
}
