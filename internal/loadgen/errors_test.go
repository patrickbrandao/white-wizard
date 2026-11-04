package loadgen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestHTTPError(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"http_429/rate_limit_error": {429, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`},
		"http_429/requests":         {429, `{"error":{"message":"x","type":"requests","code":"rate_limit_exceeded"}}`},
		"http_401/invalid_api_key":  {401, `{"error":{"message":"x","type":null,"code":"invalid_api_key"}}`},
		"http_402/402":              {402, `{"error":{"code":402,"message":"x"}}`},
		"http_500":                  {500, `{"error":{"code":{"nested":1}}}`},
		"http_502":                  {502, `<html>bad gateway</html>`},
		"http_503":                  {503, `{"error":null}`},
		"http_400":                  {400, ``},
	}
	for want, c := range cases {
		if got := HTTPError(c.status, []byte(c.body)); got != want {
			t.Errorf("%d %s: %q want %q", c.status, c.body, got, want)
		}
		if !IsHTTPError(want) {
			t.Errorf("IsHTTPError(%q)", want)
		}
	}
	if IsHTTPError(ErrTimeout) || StreamError("") != "stream_error/unknown" || StreamError("x") != "stream_error/x" {
		t.Fatal("helpers")
	}
}

func TestClassifyTransport(t *testing.T) {
	bg := context.Background()
	canceled, cancel := context.WithCancel(bg)
	cancel()
	expired, cancel2 := context.WithTimeout(bg, 0)
	defer cancel2()
	cases := []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{canceled, io.EOF, ErrCanceled},
		{expired, io.EOF, ErrTimeout},
		{bg, fasthttp.ErrDialTimeout, ErrTimeout},
		{bg, fasthttp.ErrNoFreeConns, ErrTimeout},
		{bg, fasthttp.ErrTimeout, ErrTimeout},
		{bg, os.ErrDeadlineExceeded, ErrTimeout},
		{bg, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, ErrTimeout},
		{bg, &net.DNSError{Err: "no such host", Name: "x"}, ErrConnect},
		{bg, &net.OpError{Op: "dial", Err: errors.New("x")}, ErrConnect},
		{bg, fmt.Errorf("w: %w", syscall.ECONNREFUSED), ErrConnect},
		{bg, tls.RecordHeaderError{Msg: "x"}, ErrConnect},
		{bg, tls.AlertError(40), ErrConnect},
		{bg, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, ErrConnect},
		{bg, errors.New("tls: handshake failure"), ErrConnect},
		{bg, errors.New("x509: certificate signed by unknown authority"), ErrConnect},
		{bg, io.ErrUnexpectedEOF, ErrReset},
		{bg, syscall.ECONNRESET, ErrReset},
		{bg, fasthttp.ErrConnectionClosed, ErrReset},
	}
	for _, c := range cases {
		if got := classifyTransport(c.ctx, c.err); got != c.want {
			t.Errorf("%v: %q want %q", c.err, got, c.want)
		}
	}
}

func TestConnOfUntracked(t *testing.T) {
	if connOf(&fasthttp.Response{}) != nil {
		t.Fatal("response without a tracked connection")
	}
}
