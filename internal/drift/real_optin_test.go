package drift

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// recordTransport records the hosts asked and answers with an error (except for loopback): no
// test reaches the network.
type recordTransport struct{ hosts []string }

func (t *recordTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(r.URL.Host, "127.0.0.1") {
		t.hosts = append(t.hosts, r.URL.Host)
		return nil, errors.New("sem rede no teste")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestRunRealNeedsOptIn(t *testing.T) {
	t.Chdir(t.TempDir())
	upstream := startEmbedded(t)
	run := func(real bool) (*Report, []string) {
		tr := &recordTransport{}
		rep, err := Run(context.Background(), Options{
			Providers: []string{ProviderOpenAI}, Probes: []string{ProbeModels},
			Keys: map[string]string{ProviderOpenAI: sentinel}, WWZURL: upstream.URL,
			Real: real, Client: &http.Client{Transport: tr},
		})
		if err != nil {
			t.Fatal(err)
		}
		return rep, tr.hosts
	}
	rep, hosts := run(false)
	oa := providerReport(t, rep, ProviderOpenAI)
	if oa.Status != StatusSkipped || oa.Reason != ReasonRealNotConfirmed || oa.Base != "https://api.openai.com" {
		t.Errorf("sem --real: %+v", oa)
	}
	if slices.Contains(hosts, "api.openai.com") {
		t.Errorf("sem --real chamou o real: %v", hosts)
	}
	rep, hosts = run(true)
	oa = providerReport(t, rep, ProviderOpenAI)
	if oa.Base != "https://api.openai.com" || !slices.Contains(hosts, "api.openai.com") {
		t.Errorf("com --real: %+v hosts %v", oa, hosts)
	}
	if _, txt := render(t, rep); !strings.Contains(txt, "base=https://api.openai.com") {
		t.Errorf("texto sem a base:\n%s", txt)
	}
}

// TestRunAzureNeedsBase: azure has no real base (ADR 0043): with a key and
// --real but without --base-url it is skipped/no_key and nothing leaves.
func TestRunAzureNeedsBase(t *testing.T) {
	t.Chdir(t.TempDir())
	upstream := startEmbedded(t)
	tr := &recordTransport{}
	rep, err := Run(context.Background(), Options{
		Providers: []string{ProviderAzure}, Keys: map[string]string{ProviderAzure: sentinel},
		WWZURL: upstream.URL, Real: true, Client: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := providerReport(t, rep, ProviderAzure); p.Status != StatusSkipped || p.Reason != ReasonNoKey || p.Base != "" || len(tr.hosts) != 0 {
		t.Errorf("%+v hosts %v", p, tr.hosts)
	}
}
