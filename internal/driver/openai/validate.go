package openai

import (
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai/oaicheck"
)

var _ driver.RequestValidator = (*Driver)(nil)

// ValidateRequest refuses what the real API refuses (oaicheck, spec 004).
func (d *Driver) ValidateRequest(kind driver.Kind, body []byte, _ driver.Headers, models driver.Models) *core.Error {
	return oaicheck.Validate(models, kind, body)
}
