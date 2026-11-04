package scenario

import (
	"errors"
	"testing"
)

func TestMatchesWithoutMatchBlock(t *testing.T) {
	s := &Scenario{Name: "n"}
	if !s.matches(userReq("anything")) {
		t.Fatal("a scenario without match applies to every request")
	}
}

func TestSemanticMatchUnmarshalFailure(t *testing.T) {
	var m SemanticMatch
	boom := errors.New("boom")
	if err := m.UnmarshalYAML(func(any) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckReasoningTemplate(t *testing.T) {
	rep := checkYAML(t, "name: r\nsteps:\n  - reasoning: 'think {{ nope'\n    content: ok\n", CheckOptions{})
	if !hasMsg(rep.Errors, "reasoning: invalid template") {
		t.Fatalf("report = %+v", rep)
	}
}
