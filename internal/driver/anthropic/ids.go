package anthropic

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

var _ driver.IDMinter = (*Driver)(nil)

// RequestID returns "req_011C…", the request-id header and error request_id.
func (d *Driver) RequestID(_ driver.Kind, unique string) string {
	return ids.AnthropicRequest(ids.FromToken(unique))
}

// ResponseID returns "msg_011C…".
func (d *Driver) ResponseID(_ driver.Kind, rng *generate.RNG, _ time.Time) string {
	return ids.AnthropicMessage(rng)
}

// ToolCallID returns "toolu_01…".
func (d *Driver) ToolCallID(_ driver.Kind, rng *generate.RNG) string {
	return ids.AnthropicToolUse(rng)
}
