package admin

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

func TestApplyValidateAndBadRequestError(t *testing.T) {
	s := newServer(t, fastCfg, func(o *Options) {
		o.Load = func() (*config.Config, error) { return &config.Config{}, nil }
	})
	if err := s.Apply(&config.Config{}); err == nil {
		t.Fatal("invalid config must be refused")
	}
	if _, err := s.Reload(); err == nil {
		t.Fatal("Reload must refuse an invalid config")
	}
	if got := (&badRequest{msg: "m"}).Error(); got != "m" {
		t.Fatalf("Error() = %q", got)
	}
}
