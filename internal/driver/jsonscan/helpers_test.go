package jsonscan

import "testing"

func TestLookupPresentAndHelpers(t *testing.T) {
	fs, _ := Members([]byte(`{"a":null,"b":"x","c":2.5}`))
	if v, ok := Lookup(fs, "a"); !ok || string(v) != "null" {
		t.Fatalf("lookup a: %q %v", v, ok)
	}
	if _, ok := Lookup(fs, "z"); ok {
		t.Fatal("lookup of a missing key")
	}
	if _, ok := Present(fs, "a"); ok {
		t.Fatal("null is not present")
	}
	if v, ok := Present(fs, "b"); !ok || string(v) != `"x"` {
		t.Fatalf("present b: %q %v", v, ok)
	}
	if s, ok := StringValue([]byte(`"x"`)); !ok || s != "x" {
		t.Fatalf("string: %q %v", s, ok)
	}
	if _, ok := StringValue([]byte(`1`)); ok {
		t.Fatal("number as string")
	}
	if f, ok := NumberValue([]byte(` 2.5 `)); !ok || f != 2.5 {
		t.Fatalf("number: %v %v", f, ok)
	}
	if Quote(`a"b`) != `"a\"b"` {
		t.Fatal("quote")
	}
	if s := Set("a", "b"); len(s) != 2 {
		t.Fatalf("set: %v", s)
	}
}
