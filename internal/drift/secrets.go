package drift

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mask replaces secrets in every output.
const Mask = "***"

// Redactor hides a fixed set of secrets.
type Redactor struct {
	secrets []string
}

// NewRedactor builds a redactor for the non-empty secrets.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
	// Longest first, so a secret containing another is masked whole.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	return r
}

// String masks every secret in s.
func (r *Redactor) String(s string) string {
	for _, k := range r.secrets {
		s = strings.ReplaceAll(s, k, Mask)
	}
	return s
}

// Bytes masks every secret in b.
func (r *Redactor) Bytes(b []byte) []byte {
	for _, k := range r.secrets {
		b = bytes.ReplaceAll(b, []byte(k), []byte(Mask))
	}
	return b
}

// LoadKeys returns the real key of each provider, by name. Keys come from
// the KEY=value file at path ("#" comments, optional "export " and quotes)
// and the environment variable of the same name wins. A missing file is
// not an error unless explicit is set. getenv nil means os.Getenv.
func LoadKeys(path string, explicit bool, getenv func(string) string) (map[string]string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	file := map[string]string{}
	if path != "" {
		data, err := os.ReadFile(filepath.Clean(path))
		switch {
		case errors.Is(err, fs.ErrNotExist) && !explicit:
		case err != nil:
			// The path is not secret; the error never carries file content.
			return nil, fmt.Errorf("drift: arquivo de chaves: %w", err)
		default:
			file = parseEnvFile(data)
		}
	}
	keys := map[string]string{}
	for _, name := range ProviderNames {
		for _, v := range append([]string{providers[name].EnvKey}, providers[name].EnvAlt...) {
			if k := getenv(v); k != "" {
				keys[name] = k
			} else if k := file[v]; k != "" {
				keys[name] = k
			}
			if keys[name] != "" {
				break
			}
		}
	}
	return keys, nil
}

func parseEnvFile(data []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		m[strings.TrimSpace(k)] = v
	}
	return m
}
