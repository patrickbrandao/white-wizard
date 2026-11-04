package jsonscan

import "testing"

// Moved from internal/server with the validators (task 388).

func TestMembersAndKinds(t *testing.T) {
	fs, ok := Members([]byte(` {"a" : 1 , "b\u0021":"x\"}" ,"c":[1,{"d":"]"}], "e":{} } `))
	if !ok || len(fs) != 4 || fs[1].Key != "b!" || string(fs[1].Val) != `"x\"}"` || string(fs[2].Val) != `[1,{"d":"]"}]` || string(fs[3].Val) != `{}` {
		t.Fatalf("members: %v %q", ok, fs)
	}
	if _, ok := Members([]byte(`[1]`)); ok {
		t.Fatal("array as object")
	}
	if _, ok := Members(nil); ok {
		t.Fatal("empty as object")
	}
	if es := Elements([]byte(`[ 1 , "a,]" , [2] ]`)); len(es) != 3 || string(es[1]) != `"a,]"` {
		t.Fatalf("elements: %q", es)
	}
	if es := Elements([]byte(`{}`)); es != nil {
		t.Fatalf("elements of object: %q", es)
	}
	kinds := map[string]Kind{``: Invalid, `null`: Null, `true`: Bool, `false`: Bool, `-1`: Int, `1.5`: Float, `1e3`: Float, `"x"`: String, `[]`: Array, `{}`: Object}
	for in, want := range kinds {
		if got := KindOf([]byte(in)); got != want {
			t.Errorf("KindOf(%q) = %d want %d", in, got, want)
		}
	}
	if SkipValue([]byte(`x`), 5) != 5 || SkipValue([]byte(`{"a":1`), 0) != 6 || SkipString([]byte(`"abc`), 0) != 4 {
		t.Fatal("skip at end of input")
	}
	if _, ok := NumberValue([]byte(`"1"`)); ok {
		t.Fatal("string as number")
	}
	if _, ok := StringValue([]byte(`"\x"`)); ok {
		t.Fatal("bad escape decoded")
	}
}
