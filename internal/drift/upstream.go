package drift

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// Forms of the OpenCode chat "id" (spec 006, 16.3; ADR 0059). The gateway
// forwards each session to an upstream and the id changes with it.
const (
	formGW       = "gw"
	formHex32    = "hex32"
	formChatcmpl = "chatcmpl"
	formUnknown  = "unknown"
)

// MaxNewSessions is how many new sessions the OpenCode probe tries when the
// real chat answers with an upstream other than the gw- one.
const MaxNewSessions = 3

var (
	gwID    = regexp.MustCompile(`^gw-[0-9a-f]{12}-[0-9a-f]{6}$`)
	hex32ID = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// upstreamForm classifies an OpenCode chat id. Shapes carry only the type,
// so the id is never a divergence; the form tells which upstream answered.
func upstreamForm(id string) string {
	switch {
	case gwID.MatchString(id):
		return formGW
	case hex32ID.MatchString(id):
		return formHex32
	case strings.HasPrefix(id, "chatcmpl-"):
		return formChatcmpl
	}
	return formUnknown
}

// bodyID returns the top-level "id" string of a JSON body ("" otherwise).
func bodyID(body []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.ID
}

// changeSession is called when the real chat answered with an id that is
// not the gw- form (chatcmpl-, 32 hex or any other upstream, whose bodies
// differ from the DeepSeek one wwz reproduces). It tries up to MaxNewSessions new sessions (one real chat call
// each) and, on the first that answers with the gw- form wwz reproduces,
// repeats the chat probe on it. It returns the probe result, the extra real cost and whether the
// form could not be reproduced.
func (r *runner) changeSession(ctx context.Context, p Provider, key, model string, first ProbeReport) (res ProbeReport, cost float64, unreproduced bool) {
	for range MaxNewSessions {
		r.session = r.newSession()
		r.realID = ""
		c, err := r.tryForm(ctx, p, key, model)
		cost += c
		if err != nil {
			first.Status, first.Reason = StatusError, "real: "+r.red.String(err.Error())
			return first, cost, false
		}
		if upstreamForm(r.realID) == formGW {
			res, c = r.probe(ctx, p, ProbeChat, key, model)
			return res, cost + c, false
		}
	}
	return first, cost, true
}

// tryForm sends the chat probe to the real provider only, recording the id
// in r.realID, and returns its cost.
func (r *runner) tryForm(ctx context.Context, p Provider, key, model string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	a, err := r.do(ctx, p.BaseURL, p, p.probeRequest(ProbeChat, model), key)
	if err != nil {
		return 0, err
	}
	if a.status/100 != 2 {
		return 0, nil
	}
	r.realID = bodyID(a.body)
	in, out, ok := usageOf(a.body, false)
	return CostUSD(model, in, out, ok), nil
}
