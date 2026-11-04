// Package fakevalkey is a tiny scripted RESP3 server for tests of the Valkey
// store: it answers the connection handshake and SCRIPT LOAD by itself and
// hands every other command to a handler, so a test can answer with errors
// and malformed replies that a real server never produces. It does not
// import internal/limits, so in-package tests can use it too.
package fakevalkey

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // G505: script id, not security
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Request is one command received by the handler. Name is the upper-cased
// command; for EVALSHA and EVAL it is "EVALSHA#<n>" / "EVAL#<n>" where n is
// the load order (0-based) of the script among the SCRIPT LOAD calls, so a
// test can tell the scripts apart without importing the store.
type Request struct {
	Name string
	Args []string // arguments after the command name
}

// CloseConn, returned by a handler, drops the connection without a reply.
const CloseConn = "\x00close"

// Handler answers a command with a raw RESP reply (see the helpers). An
// empty string means "-ERR fake: unhandled <name>".
type Handler func(Request) string

// Server is the scripted server.
type Server struct {
	Addr string

	ln net.Listener
	mu sync.Mutex
	h  Handler

	scripts map[string]int // sha -> load index
	srcs    []string
	conns   map[net.Conn]struct{}
	calls   []Request
}

// listen is net.Listen; a test replaces it to reach the failure branch.
var listen = net.Listen

// New starts a server answering through h (nil = every command "+OK").
func New(t testing.TB, h Handler) *Server {
	t.Helper()
	ln, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: ln.Addr().String(), ln: ln, h: h, scripts: map[string]int{}, conns: map[net.Conn]struct{}{}}
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// SetHandler replaces the handler (safe while serving).
func (s *Server) SetHandler(h Handler) {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
}

// Calls returns the commands that reached the handler so far.
func (s *Server) Calls() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.calls...)
}

// Close stops the server and drops its connections.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		out := s.reply(args)
		if out == CloseConn {
			return
		}
		// A write to a client that went away fails; the next read ends the loop.
		_, _ = io.WriteString(c, out)
	}
}

func (s *Server) reply(args []string) string {
	name := strings.ToUpper(args[0])
	switch name {
	case "HELLO":
		return "%7\r\n+server\r\n+valkey\r\n+version\r\n+8.1.0\r\n+proto\r\n:3\r\n+id\r\n:1\r\n+mode\r\n+standalone\r\n+role\r\n+master\r\n+modules\r\n*0\r\n"
	case "SELECT", "CLIENT":
		return OK()
	case "SCRIPT":
		if len(args) >= 3 && strings.EqualFold(args[1], "LOAD") {
			sum := sha1.Sum([]byte(args[2])) //nolint:gosec // G401: script id, not security
			sha := hex.EncodeToString(sum[:])
			s.mu.Lock()
			if _, ok := s.scripts[sha]; !ok {
				s.scripts[sha] = len(s.srcs)
				s.srcs = append(s.srcs, args[2])
			}
			h := s.h
			s.mu.Unlock()
			if h != nil {
				if out := h(Request{Name: "SCRIPT LOAD", Args: args[2:]}); out != "" {
					return out
				}
			}
			return Bulk(sha)
		}
	}
	rq := Request{Name: name, Args: args[1:]}
	switch name {
	case "EVALSHA":
		s.mu.Lock()
		idx, ok := s.scripts[args[1]]
		s.mu.Unlock()
		if !ok {
			return Err("NOSCRIPT No matching script")
		}
		rq.Name = "EVALSHA#" + strconv.Itoa(idx)
		rq.Args = args[3:]
	case "EVAL":
		s.mu.Lock()
		idx := -1
		for i, src := range s.srcs {
			if src == args[1] {
				idx = i
			}
		}
		s.mu.Unlock()
		rq.Name = "EVAL#" + strconv.Itoa(idx)
		rq.Args = args[3:]
	}
	s.mu.Lock()
	h := s.h
	s.calls = append(s.calls, rq)
	s.mu.Unlock()
	if h == nil {
		return OK()
	}
	if out := h(rq); out != "" {
		return out
	}
	return Err("ERR fake: unhandled " + rq.Name)
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return strings.Fields(line), nil
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(hdr, "\r\n")[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

// OK is "+OK".
func OK() string { return "+OK\r\n" }

// Err is an error reply ("-" + msg).
func Err(msg string) string { return "-" + msg + "\r\n" }

// Int is an integer reply.
func Int(n int64) string { return fmt.Sprintf(":%d\r\n", n) }

// Bulk is a bulk string reply.
func Bulk(s string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s) }

// Null is the RESP3 null.
func Null() string { return "_\r\n" }

// Arr is an array of already encoded elements.
func Arr(elems ...string) string {
	return fmt.Sprintf("*%d\r\n%s", len(elems), strings.Join(elems, ""))
}

// Ints is an array of integer replies.
func Ints(ns ...int64) string {
	e := make([]string, len(ns))
	for i, n := range ns {
		e[i] = Int(n)
	}
	return Arr(e...)
}

// Strs is an array of bulk strings.
func Strs(ss ...string) string {
	e := make([]string, len(ss))
	for i, s := range ss {
		e[i] = Bulk(s)
	}
	return Arr(e...)
}

// Map is a RESP3 map of bulk strings (key, value, key, value...).
func Map(kv ...string) string {
	e := make([]string, len(kv))
	for i, s := range kv {
		e[i] = Bulk(s)
	}
	return fmt.Sprintf("%%%d\r\n%s", len(kv)/2, strings.Join(e, ""))
}
