package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"
)

// VaultClient talks to clawvault over its Unix socket. It can send prompts
// and check status — it can NEVER retrieve a key (the socket has no such op).
// When CLAWVAULT_SOCKET is set, nanoclaw holds no wallet secret and routes all
// wallet work here; /connect and confirmation live in clawvault's own bot.

type VaultClient struct {
	path string
	mu   sync.Mutex // one request/response at a time per dial
}

func NewVaultClient(path string) *VaultClient {
	if path == "" {
		return nil
	}
	return &VaultClient{path: path}
}

type vcResp struct {
	OK        bool   `json:"ok"`
	Connected bool   `json:"connected"`
	Result    string `json:"result"`
	Queued    bool   `json:"queued"`
	Error     string `json:"error"`
}

func (c *VaultClient) call(req map[string]string) (vcResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn, err := net.DialTimeout("unix", c.path, 5*time.Second)
	if err != nil {
		return vcResp{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(120 * time.Second)) // Bankr reads can take ~90s
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return vcResp{}, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() {
		return vcResp{}, errors.New("no response from vault")
	}
	var r vcResp
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		return vcResp{}, err
	}
	return r, nil
}

func (c *VaultClient) Connected(uid string) bool {
	r, err := c.call(map[string]string{"op": "status", "uid": uid})
	return err == nil && r.Connected
}

// Prompt returns (text, queued, error). queued=true means clawvault will show
// the user its own Confirm button; text is empty in that case.
func (c *VaultClient) Prompt(uid, channel, text string) (string, bool, error) {
	r, err := c.call(map[string]string{"op": "prompt", "uid": uid, "channel": channel, "text": text})
	if err != nil {
		return "", false, err
	}
	if r.Error != "" {
		return "", false, errors.New(r.Error)
	}
	return r.Result, r.Queued, nil
}
