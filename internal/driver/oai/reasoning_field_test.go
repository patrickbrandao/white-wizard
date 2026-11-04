package oai

import "testing"

func TestGatewayReasoningField(t *testing.T) {
	for _, v := range []string{"reasoning_content", "reasoning", "hidden", "inline_tag"} {
		if got, err := GatewayReasoningField(v, "x"); err != nil || got != v {
			t.Errorf("%q: %q %v", v, got, err)
		}
	}
	if got, err := GatewayReasoningField("", "reasoning"); err != nil || got != "reasoning" {
		t.Errorf("default: %q %v", got, err)
	}
	if _, err := GatewayReasoningField("thoughts", "x"); err == nil {
		t.Error("invalid value accepted")
	}
}
