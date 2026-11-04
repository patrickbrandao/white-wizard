package oai

import "fmt"

// GatewayReasoningField resolves the reasoning_field option of a gateway
// driver (opencode, openrouter): the formats the real gateways use to imitate
// the upstream model (spec 007, ADR 0046). An empty value yields def.
func GatewayReasoningField(v, def string) (string, error) {
	switch v {
	case "":
		return def, nil
	case "reasoning_content", "reasoning", ReasoningHidden, "inline_tag":
		return v, nil
	}
	return "", fmt.Errorf("reasoning_field %q is not accepted (reasoning_content, reasoning, hidden, inline_tag)", v)
}
