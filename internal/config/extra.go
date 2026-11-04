package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// validateExtra rejects a models[i].extra key that no dialect serving the
// model has in its /models item. The keys of each item come from the
// driver that renders it (driver.Declarer, spec 005).
func (m *ModelConfig) validateExtra(i int) error {
	return m.checkExtraKeys(fmt.Sprintf("models[%d].extra", i), m.Extra)
}

// checkExtraKeys rejects a key of extra, an extra block of the model at
// path (models[i].extra or models[i].catalogs.<c>.extra), that no dialect
// serving the model has in its /models item.
func (m *ModelConfig) checkExtraKeys(path string, extra map[string]any) error {
	if len(extra) == 0 {
		return nil
	}
	var dialects []string
	allowed := map[string]bool{}
	for _, d := range declaredDrivers() {
		decl, _ := declOf(d)
		for _, dia := range decl.Dialects {
			if !m.ServedOn(d, dia) {
				continue
			}
			if !slices.Contains(dialects, dia) {
				dialects = append(dialects, dia)
			}
			for _, k := range decl.ModelExtra {
				allowed[k] = true
			}
		}
	}
	slices.Sort(dialects)
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		if !allowed[k] {
			return fmt.Errorf("config: %s: key %q unknown to dialects [%s]", path, k, strings.Join(dialects, " "))
		}
	}
	return nil
}
