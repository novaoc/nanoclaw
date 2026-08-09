package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Out-of-band confirmation for fund-moving Bankr actions. The model can
// REQUEST a write, but it cannot approve one: approval is a Discord button
// click by the original requester, verified in Go. This closes the prompt-
// injection path — a poisoned web page fetched via fetch_url can put text in
// the model's context, but it cannot click a button as the user.

const confirmTTL = 5 * time.Minute

var (
	errConfirmExpired = errors.New("this confirmation expired or was already used")
	errConfirmNotYours = errors.New("only the person who requested this can confirm it")
)

type PendingAction struct {
	Token   string
	UID     string // Discord id of the requester — only they may confirm
	Prompt  string // the exact instruction that will run on confirm
	created time.Time
}

type Confirmations struct {
	mu sync.Mutex
	m  map[string]*PendingAction
}

func NewConfirmations() *Confirmations { return &Confirmations{m: map[string]*PendingAction{}} }

func randToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Confirmations) Add(uid, prompt string) *PendingAction {
	a := &PendingAction{Token: randToken(), UID: uid, Prompt: prompt, created: time.Now()}
	c.mu.Lock()
	c.m[a.Token] = a
	// opportunistic sweep of anything stale
	for tok, p := range c.m {
		if time.Since(p.created) > confirmTTL {
			delete(c.m, tok)
		}
	}
	c.mu.Unlock()
	return a
}

// Take consumes a pending action ONLY for its owner. A wrong clicker gets
// errConfirmNotYours and the action is left intact for the real requester.
func (c *Confirmations) Take(token, uid string) (*PendingAction, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.m[token]
	if a == nil {
		return nil, errConfirmExpired
	}
	if a.UID != uid {
		return nil, errConfirmNotYours
	}
	delete(c.m, token)
	if time.Since(a.created) > confirmTTL {
		return nil, errConfirmExpired
	}
	return a, nil
}

// Cancel drops a pending action (owner only).
func (c *Confirmations) Cancel(token, uid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.m[token]
	if a == nil {
		return errConfirmExpired
	}
	if a.UID != uid {
		return errConfirmNotYours
	}
	delete(c.m, token)
	return nil
}
