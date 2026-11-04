package gemini

import (
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

var _ driver.IDMinter = (*Driver)(nil)

// RequestID is internal: the Gemini API sends no request id.
func (d *Driver) RequestID(_ driver.Kind, unique string) string {
	return ids.OpenAIRequest(ids.FromToken(unique))
}

// ResponseID is responseId: base64url without padding of 17 bytes, the
// first 4 the Unix seconds little-endian (observed: "Zv7latSxD4aiuMYP487m6A8"
// decodes to 1793457766 followed by 13 opaque bytes).
func (d *Driver) ResponseID(_ driver.Kind, rng *generate.RNG, now time.Time) string {
	b := make([]byte, 17)
	binary.LittleEndian.PutUint32(b, uint32(now.Unix())) //nolint:gosec // G115: Unix seconds fit until 2106
	for i := 4; i < len(b); i++ {
		b[i] = byte(rng.IntN(256)) //nolint:gosec // G115: IntN(256) fits a byte
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ToolCallID is the functionCall id: "call_" and 6 digits (observed:
// call_441933, call_303031).
func (d *Driver) ToolCallID(_ driver.Kind, rng *generate.RNG) string {
	n := strconv.Itoa(rng.IntN(1000000))
	for len(n) < 6 {
		n = "0" + n
	}
	return "call_" + n
}
