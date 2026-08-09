package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
)

// Unix-socket API for nanoclaw. Minimal by design — the ONLY ops are status,
// prompt, and disconnect. There is no "get key" and never will be. The socket
// file is 0600 owned by clawvault's user; nanoclaw's user is granted access
// via group or an explicit chmod at deploy time.

var (
	errNotConnected = errors.New("not connected")
	errBadRequest   = func(s string) error { return errors.New(s) }
)

type request struct {
	Op      string `json:"op"`
	UID     string `json:"uid"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

type response struct {
	OK        bool   `json:"ok"`
	Connected bool   `json:"connected,omitempty"`
	Result    string `json:"result,omitempty"`
	Queued    bool   `json:"queued,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Server struct {
	vault *Vault
	path  string
}

func NewServer(v *Vault, path string) *Server { return &Server{vault: v, path: path} }

func (s *Server) Serve() error {
	_ = os.Remove(s.path) // stale socket from a prior run
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	_ = os.Chmod(s.path, 0o660) // owner + group; deploy grants nanoclaw the group
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = enc.Encode(response{Error: "bad request"})
			continue
		}
		_ = enc.Encode(s.dispatch(req))
	}
}

func (s *Server) dispatch(req request) response {
	switch req.Op {
	case "status":
		return response{OK: true, Connected: s.vault.Connected(req.UID)}
	case "disconnect":
		return response{OK: s.vault.Disconnect(req.UID)}
	case "prompt":
		r, err := s.vault.Prompt(req.UID, req.Channel, req.Text)
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Result: r.Result, Queued: r.Queued}
	}
	return response{Error: "unknown op"}
}
