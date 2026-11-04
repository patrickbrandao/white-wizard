package driver

import (
	"errors"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// undeclared is a driver with no optional interface.
type undeclared struct{ Driver }

// TestDeclOfWithoutDeclaration: a driver that cannot be built without
// options, or that declares nothing, publishes no declaration.
func TestDeclOfWithoutDeclaration(t *testing.T) {
	if _, ok := declOf(func(map[string]string) (Driver, error) { return nil, errors.New("needs options") }); ok {
		t.Fatal("factory error declared")
	}
	if _, ok := declOf(func(map[string]string) (Driver, error) { return nil, nil }); ok {
		t.Fatal("nil driver declared")
	}
	if _, ok := declOf(func(map[string]string) (Driver, error) { return undeclared{}, nil }); ok {
		t.Fatal("driver without Declarer declared")
	}
}

func TestRawReply(t *testing.T) {
	r := RawReply{Status: 418, ContentType: "text/plain", Body: []byte("x")}
	e := RawError("teapot", r)
	if e.Kind != core.ErrInvalidRequest || e.Status != 418 || e.Message != "teapot" {
		t.Fatalf("%+v", e)
	}
	if got, ok := RawReplyOf(e); !ok || got.Status != 418 || string(got.Body) != "x" {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := RawReplyOf(nil); ok {
		t.Fatal("nil error has a reply")
	}
	if _, ok := RawReplyOf(&core.Error{}); ok {
		t.Fatal("plain error has a reply")
	}
}
