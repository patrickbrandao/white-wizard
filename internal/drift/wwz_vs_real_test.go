package drift

// The current wwz against the recorded real answers (task 387; spec 006,
// "wwz-drift check sem divergência"; ADR 0014): the probes of wwz-drift
// check go to the embedded wwz of this build, and each answer is compared
// with testdata/real/<provider>/<probe>-real.txt by Compare, without a real
// key. The frozen "-wwz" files of the first drift stay as the regression of
// the comparison itself (real_test.go); this test holds the drivers.

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestWWZAgainstRecordedReal(t *testing.T) {
	w, err := startWith(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	o := Options{Client: &http.Client{Timeout: 30 * time.Second}}
	o.defaults()
	r := &runner{o: o, red: NewRedactor(), session: o.Session}
	for _, name := range recorded() {
		p := providers[name]
		for _, probe := range ProbeNames {
			if responsesProbe(probe) && p.ResponsesPath == "" {
				continue
			}
			t.Run(name+"/"+probe, func(t *testing.T) {
				key := DefaultWWZKey
				if probe == ProbeErrorAuth {
					key = InvalidKey
				}
				wa, err := r.do(context.Background(), w.URL+p.WWZPrefix, p, p.probeRequest(probe, p.DefaultModel), key)
				if err != nil {
					t.Fatal(err)
				}
				var ws Signature
				if probe == ProbeModels {
					ws, err = SignModels(wa.status, wa.header, wa.body, p.DefaultModel)
				} else {
					ws, err = Sign(wa.status, wa.header, wa.body)
				}
				if err != nil {
					t.Fatalf("wwz %d %s: %v", wa.status, wa.body, err)
				}
				ds := Compare(rawSignature(t, name, probe, "real"), ws)
				if !markIgnored(DefaultIgnores(), name, probe, ds) {
					return
				}
				t.Errorf("wwz diverges from the recorded real answer: %s\nwwz: HTTP %d %s", kinds(ds), wa.status, wa.body)
			})
		}
	}
}
