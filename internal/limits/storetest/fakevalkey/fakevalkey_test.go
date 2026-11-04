package fakevalkey

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // G505: script id, not security
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func dial(t *testing.T, s *Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c, bufio.NewReader(c)
}

func send(t *testing.T, c net.Conn, args ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	if _, err := c.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
}

// line reads one reply line (without CRLF).
func line(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	s, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(s, "\r\n")
}

func TestServerScriptedReplies(t *testing.T) {
	s := New(t, func(r Request) string {
		switch r.Name {
		case "SCRIPT LOAD":
			if strings.Contains(r.Args[0], "own") {
				return Bulk("custom")
			}
			return ""
		case "PING":
			return Bulk("pong")
		case "UNKNOWN":
			return ""
		}
		return OK()
	})
	c, r := dial(t, s)

	send(t, c, "HELLO", "3")
	if got := line(t, r); got != "%7" {
		t.Fatalf("HELLO = %q", got)
	}
	for range 14 {
		line(t, r)
	}
	send(t, c, "SELECT", "1")
	if got := line(t, r); got != "+OK" {
		t.Fatalf("SELECT = %q", got)
	}
	send(t, c, "SCRIPT", "LOAD", "return 1")
	line(t, r)                          // $40
	sum := sha1.Sum([]byte("return 1")) //nolint:gosec // G401: script id, not security
	sha := hex.EncodeToString(sum[:])
	if got := line(t, r); got != sha {
		t.Fatalf("sha = %q", got)
	}
	send(t, c, "SCRIPT", "LOAD", "own")
	line(t, r)
	if got := line(t, r); got != "custom" {
		t.Fatalf("custom load = %q", got)
	}
	send(t, c, "EVALSHA", sha, "0")
	if got := line(t, r); got != "+OK" {
		t.Fatalf("EVALSHA = %q", got)
	}
	send(t, c, "EVALSHA", "nope", "0")
	if got := line(t, r); !strings.HasPrefix(got, "-NOSCRIPT") {
		t.Fatalf("unknown sha = %q", got)
	}
	send(t, c, "EVAL", "return 1", "0")
	if got := line(t, r); got != "+OK" {
		t.Fatalf("EVAL = %q", got)
	}
	send(t, c, "UNKNOWN")
	if got := line(t, r); !strings.HasPrefix(got, "-ERR fake: unhandled UNKNOWN") {
		t.Fatalf("unhandled = %q", got)
	}
	if _, err := c.Write([]byte("PING\r\n")); err != nil { // inline command
		t.Fatal(err)
	}
	line(t, r)
	if got := line(t, r); got != "pong" {
		t.Fatalf("inline PING = %q", got)
	}

	names := ""
	for _, c := range s.Calls() {
		names += c.Name + " "
	}
	if !strings.Contains(names, "EVALSHA#0") || !strings.Contains(names, "EVAL#0") || !strings.Contains(names, "PING") {
		t.Fatalf("calls = %s", names)
	}
}

func TestServerNoHandlerAndClose(t *testing.T) {
	s := New(t, nil)
	c, r := dial(t, s)
	send(t, c, "ANYTHING")
	if got := line(t, r); got != "+OK" {
		t.Fatalf("no handler = %q", got)
	}
	s.SetHandler(func(Request) string { return CloseConn })
	send(t, c, "DROP")
	if _, err := r.ReadString('\n'); err == nil {
		t.Fatal("connection must drop")
	}
	// Empty and malformed input is tolerated.
	c2, r2 := dial(t, s)
	if _, err := c2.Write([]byte("\r\n*x\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ReadString('\n'); err == nil {
		t.Fatal("malformed command must end the connection")
	}
	s.Close()
}

func TestEncoders(t *testing.T) {
	if Err("E") != "-E\r\n" || Int(3) != ":3\r\n" || Bulk("ab") != "$2\r\nab\r\n" || Null() != "_\r\n" {
		t.Fatal("scalars")
	}
	if Arr(Int(1)) != "*1\r\n:1\r\n" || Ints(1, 2) != "*2\r\n:1\r\n:2\r\n" || Strs("a") != "*1\r\n$1\r\na\r\n" || Map("k", "v") != "%1\r\n$1\r\nk\r\n$1\r\nv\r\n" {
		t.Fatal("aggregates")
	}
}

func TestTruncatedCommands(t *testing.T) {
	s := New(t, nil)
	for _, raw := range []string{"*1\r\n", "*1\r\n$z\r\n", "*1\r\n$5\r\nab"} {
		c, r := dial(t, s)
		if _, err := c.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if raw == "*1\r\n" || strings.HasSuffix(raw, "ab") {
			_ = c.(*net.TCPConn).CloseWrite()
		}
		if _, err := r.ReadString('\n'); err == nil {
			t.Fatalf("%q must end the connection", raw)
		}
	}
}

// failT is a testing.TB whose Fatal ends the goroutine, like the real one.
type failT struct {
	testing.TB
	msg  chan string
	done chan struct{}
}

func (f *failT) Helper()        {}
func (f *failT) Cleanup(func()) {}
func (f *failT) Fatal(args ...any) {
	f.msg <- fmt.Sprint(args...)
	runtime.Goexit()
}

func TestNewFailsWhenListenFails(t *testing.T) {
	old := listen
	listen = func(string, string) (net.Listener, error) { return nil, errors.New("no port") }
	defer func() { listen = old }()
	ft := &failT{msg: make(chan string, 1), done: make(chan struct{})}
	go func() {
		defer close(ft.done)
		New(ft, nil)
	}()
	<-ft.done
	if got := <-ft.msg; got != "no port" {
		t.Fatalf("Fatal(%q)", got)
	}
}
