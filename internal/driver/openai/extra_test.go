package openai_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

type extraRec struct {
	status  int
	headers map[string]string
	body    strings.Builder
}

func (r *extraRec) Status(c int)                   { r.status = c }
func (r *extraRec) Header(k, v string)             { r.headers[strings.ToLower(k)] = v }
func (r *extraRec) Write(p []byte) (int, error)    { return r.body.Write(p) }
func (r *extraRec) Flush() error                   { return nil }
func (r *extraRec) Fault(core.TransportFault) bool { return false }

// plainHeaders is a driver.Headers that is not a driver.MapHeaders.
type plainHeaders map[string]string

func (p plainHeaders) Get(n string) string { return p[n] }

const chatGetBody = `{"object":"list","data":[],"first_id":null,"last_id":null,"has_more":false}`

// TestExtraChatCompletionsGet: GET /v1/chat/completions is the real empty
// list (testdata/real/openai/routes/chat_get.json), byte for byte.
func TestExtraChatCompletionsGet(t *testing.T) {
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := driver.MapHeaders{"openai-project": "proj_abc"}
	driver.SetRoute(m, "GET", "/v1/chat/completions")
	for name, h := range map[string]driver.Headers{"map": m, "plain": plainHeaders(m)} {
		t.Run(name, func(t *testing.T) {
			w := &extraRec{headers: map[string]string{}}
			d.Extra(driver.KindResource, w, h, nil, driver.ExtraContext{})
			if w.status != 200 || w.body.String() != chatGetBody {
				t.Fatalf("status=%d body=%q", w.status, w.body.String())
			}
			if w.headers["content-type"] != "application/json" || w.headers["openai-version"] != "2020-10-01" ||
				w.headers["content-length"] != strconv.Itoa(len(chatGetBody)) || !strings.HasPrefix(w.headers["x-request-id"], "req_") ||
				w.headers["openai-organization"] == "" || w.headers["openai-processing-ms"] == "" {
				t.Fatalf("headers: %v", w.headers)
			}
			if name == "map" && w.headers["openai-project"] != "proj_abc" {
				t.Fatalf("project: %v", w.headers)
			}
			for k := range w.headers {
				if strings.HasPrefix(k, "x-ratelimit-") {
					t.Fatalf("unexpected %s", k)
				}
			}
		})
	}
}

// TestExtraOtherRouteIs404: any other auxiliary route is a 404.
func TestExtraOtherRouteIs404(t *testing.T) {
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := driver.MapHeaders{}
	driver.SetRoute(m, "PUT", "/v1/chat/completions")
	w := &extraRec{headers: map[string]string{}}
	d.Extra(driver.KindResource, w, m, nil, driver.ExtraContext{})
	if w.status != 404 {
		t.Fatalf("status=%d body=%s", w.status, w.body.String())
	}
}
