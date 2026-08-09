package main

// Self-contained key custody — clawvault is meant to be auditable in
// isolation, so it carries its own copy of the crypto rather than sharing
// a package with the assistant it protects. AES-GCM under a server secret,
// Discord id as AAD, keys dir 0700, owned by clawvault's Unix user only.

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

type KeyStore struct {
	dir   string
	aead  cipher.AEAD
	mu    sync.RWMutex
	cache map[string]string
}

var bkKeyRe = regexp.MustCompile(`^bk_[A-Za-z0-9_-]{8,120}$`)
var uidClean = regexp.MustCompile(`[^0-9A-Za-z_-]`)

func ValidBankrKey(s string) bool { return bkKeyRe.MatchString(s) }

func NewKeyStore(dir, secret string) (*KeyStore, error) {
	if secret == "" {
		return nil, errors.New("NANOCLAW_SECRET is required for clawvault")
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &KeyStore{dir: dir, aead: aead, cache: map[string]string{}}, nil
}

func (ks *KeyStore) path(uid string) string {
	return filepath.Join(ks.dir, uidClean.ReplaceAllString(uid, "")+".enc")
}

func (ks *KeyStore) Put(uid, key string) error {
	if !ValidBankrKey(key) {
		return errors.New("not a Bankr key (expected bk_…)")
	}
	nonce := make([]byte, ks.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := ks.aead.Seal(nonce, nonce, []byte(key), []byte(uid))
	if err := os.WriteFile(ks.path(uid), []byte(base64.StdEncoding.EncodeToString(sealed)), 0o600); err != nil {
		return err
	}
	ks.mu.Lock()
	ks.cache[uid] = key
	ks.mu.Unlock()
	return nil
}

func (ks *KeyStore) Get(uid string) (string, bool) {
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
		return "", false
	}
	key := string(pt)
	ks.mu.Lock()
	ks.cache[uid] = key
	ks.mu.Unlock()
	return key, true
}

func (ks *KeyStore) Has(uid string) bool { _, ok := ks.Get(uid); return ok }

func (ks *KeyStore) Delete(uid string) bool {
	ks.mu.Lock()
	delete(ks.cache, uid)
	ks.mu.Unlock()
	return os.Remove(ks.path(uid)) == nil
}
