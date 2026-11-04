package driver

import (
	"errors"
	"testing"
)

// named is a driver that only says its name.
type named struct {
	Driver
	name string
}

func (n named) Name() string { return n.name }

// engineDeclared declares an EngineDecl.
type engineDeclared struct {
	named
	decl EngineDecl
}

func (d engineDeclared) EngineDeclaration() EngineDecl { return d.decl }

// unwrapping is a wrapper that unwraps to inner.
type unwrapping struct {
	named
	inner Driver
}

func (u unwrapping) Unwrap() Driver { return u.inner }

func zeroDecl(d EngineDecl) bool {
	return d.Text == nil && d.Template == nil && !d.ClaudeTokens && d.PromptCache == nil && !d.ContentFilterAsFinish && d.MalformedFrame == nil
}

// TestEngineDeclOf (task 402): the driver's own declaration, through
// Unwrap; for a wrapper that keeps only Name(), the registered driver's,
// built once; the zero EngineDecl otherwise.
func TestEngineDeclOf(t *testing.T) {
	if !zeroDecl(EngineDeclOf(nil)) {
		t.Error("nil driver")
	}
	own := engineDeclared{named: named{name: "t402-own"}, decl: EngineDecl{ClaudeTokens: true}}
	if !EngineDeclOf(own).ClaudeTokens {
		t.Error("own declaration")
	}
	if !EngineDeclOf(unwrapping{named: named{name: "t402-wrap"}, inner: own}).ClaudeTokens {
		t.Error("through Unwrap")
	}
	if !zeroDecl(EngineDeclOf(named{name: "t402-unregistered"})) {
		t.Error("unregistered name")
	}

	builds := 0
	Register("t402-registered", func(map[string]string) (Driver, error) {
		builds++
		return engineDeclared{named: named{name: "t402-registered"}, decl: EngineDecl{ContentFilterAsFinish: true}}, nil
	})
	wrapper := named{name: "t402-registered"}
	for range 2 {
		if !EngineDeclOf(wrapper).ContentFilterAsFinish {
			t.Error("registered driver's declaration")
		}
	}
	if builds != 1 {
		t.Errorf("built %d times, want once", builds)
	}
	// Registering again drops the cached declaration.
	Register("t402-registered", func(map[string]string) (Driver, error) {
		return named{name: "t402-registered"}, nil
	})
	if !zeroDecl(EngineDeclOf(wrapper)) {
		t.Error("re-registered driver without declaration")
	}

	Register("t402-needs-options", func(map[string]string) (Driver, error) { return nil, errors.New("needs options") })
	if !zeroDecl(EngineDeclOf(named{name: "t402-needs-options"})) {
		t.Error("factory error")
	}
}
