package azure

import (
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// Limits per deployment (spec 006 16.6, quotas and limits page): the TPM
// assigned to a deployment is its limit and the RPM is derived from it,
// per model family; the requests are evaluated over a short interval of
// 1 s or 10 s, so 600 RPM admits at most 10 requests per second. Not
// conferred (ADR 0043).

// rpmPerTPM is the RPM a deployment gets per TPM, by model family: 6 per
// 1 000 on the chat models; 1 per 1 000 on o3 and o4-mini; 1 per 6 000 on
// o1; 1 per 10 000 on o3-mini, o1-mini and o3-pro. The more specific
// prefixes come first.
var rpmPerTPM = []struct {
	prefix string
	ratio  float64
}{
	{"o3-mini", 1.0 / 10000},
	{"o1-mini", 1.0 / 10000},
	{"o3-pro", 1.0 / 10000},
	{"o4-mini", 1.0 / 1000},
	{"o3", 1.0 / 1000},
	{"o1", 1.0 / 6000},
}

// ratioOf is the RPM per TPM of a model, by its version (the real model
// name) or, without one, its id.
func ratioOf(m *config.ModelConfig) float64 {
	name := m.Version
	if name == "" {
		name = m.ID
	}
	for _, r := range rpmPerTPM {
		if strings.HasPrefix(name, r.prefix) {
			return r.ratio
		}
	}
	return 6.0 / 1000
}

// DeploymentLimits shapes the tier's count limits into the limits of one
// deployment: with a TPM, the RPM is derived from it (at least 1),
// whatever the tier says; without, the tier's RPM stays. The requests are
// evaluated over 1 s when that admits at least one request, else over
// 10 s. The rps unit of the tier does not exist on Azure: the window takes
// its place.
func (d *Driver) DeploymentLimits(m *config.ModelConfig, l config.Limits) (config.Limits, time.Duration) {
	if l.TPM > 0 {
		l.RPM = max(1, int(float64(l.TPM)*ratioOf(m)))
	}
	l.RPS, l.Burst = 0, 0
	if l.RPM <= 0 {
		return l, 0
	}
	if l.RPM >= 60 {
		return l, time.Second
	}
	return l, 10 * time.Second
}
