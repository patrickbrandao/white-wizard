package drivertest

import (
	"io/fs"
	"testing"
)

// SwapTestdata replaces the request bodies the suite reads (the embedded
// files) and returns the function that puts them back.
func SwapTestdata(f fs.ReadFileFS) (restore func()) {
	old := testdata
	testdata = f
	return func() { testdata = old }
}

func ReadTestdata(t *testing.T, name string) []byte { return readTestdata(t, name) }

func Variant(t *testing.T, file, name string) []byte { return variant(t, file, name) }

func InvalidBodies(t *testing.T, key string) map[string]string { return invalidBodies(t, key) }
