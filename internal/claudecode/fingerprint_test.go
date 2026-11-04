package claudecode

import (
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	cases := []struct {
		text, version, want string
	}{
		// Spec 020, acceptance criteria.
		{"say ok", "2.1.308", "7f2"},
		{"Hello there, reply with ok", "2.1.308", "9fe"},
		{"Second conversation, first turn", "2.1.308", "798"},
		{"quota", "2.1.308", "a39"},
		// The version enters the hash.
		{"say ok", "2.1.309", "b01"},
		// Missing indices become "0".
		{"", "2.1.308", "084"},
		// Indices count UTF-16 units: é is one unit, an astral rune two, and
		// each half of a pair becomes U+FFFD (Node's Buffer).
		{"abcdéfgh ijklmnopqrstuvwxyz", "2.1.308", "ec0"},
		{"abcdefghijklmnopqrstuvwxyz", "2.1.308", "d94"},
		{"abcd\U0001F600fgh", "2.1.308", "faf"},
		{"abcd�efg", "2.1.308", "faf"},
		{"\U0001F600\U0001F600xabcdefghijklmnopqrst", "2.1.308", "a14"},
		// A lone surrogate (WTF-8) and an invalid byte are one unit, U+FFFD.
		{"abcd\xed\xa0\x80efg", "2.1.308", "faf"},
		{"abcd\xffefg", "2.1.308", "faf"},
	}
	for _, c := range cases {
		if got := Fingerprint(c.text, c.version); got != c.want {
			t.Errorf("Fingerprint(%q, %s) = %s, want %s", c.text, c.version, got, c.want)
		}
	}
	// Text past index 20 does not matter.
	long := "abcdefghijklmnopqrstuvwxyz"
	if Fingerprint(long, "2.1.308") != Fingerprint(long[:21]+strings.Repeat("z", 1000), "2.1.308") {
		t.Error("text after index 20 changed the fingerprint")
	}
}

func TestFirstUserText(t *testing.T) {
	user := func(content Value) Value {
		return Object(M("role", String("user")), M("content", content))
	}
	cases := []struct {
		name     string
		messages []Value
		want     string
	}{
		{"none", nil, ""},
		{"string", []Value{user(String("hi"))}, "hi"},
		{"skips other roles", []Value{
			Object(M("role", String("system")), M("content", String("env"))),
			user(String("first")), user(String("second")),
		}, "first"},
		{"first text block", []Value{user(Array(
			Object(M("type", String("image"))),
			TextBlock("block", Null()),
			TextBlock("later", Null()),
		))}, "block"},
		{"no text block", []Value{user(Array(Object(M("type", String("image")))))}, ""},
	}
	for _, c := range cases {
		if got := FirstUserText(c.messages); got != c.want {
			t.Errorf("%s: FirstUserText = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBillingHeader(t *testing.T) {
	got := BillingHeader("2.1.308", EntrypointSDKCLI, "Hello there, reply with ok")
	want := "x-anthropic-billing-header: cc_version=2.1.308.9fe; cc_entrypoint=sdk-cli;"
	if got != want {
		t.Fatalf("BillingHeader = %q, want %q", got, want)
	}
	b, err := ParseBillingHeader(got)
	if err != nil || b != (Billing{Version: "2.1.308", Fingerprint: "9fe", Entrypoint: "sdk-cli"}) {
		t.Fatalf("ParseBillingHeader = %+v, %v", b, err)
	}
	bad := map[string]string{
		"no prefix":         "cc_version=2.1.308.9fe; cc_entrypoint=cli;",
		"no final ;":        "x-anthropic-billing-header: cc_version=2.1.308.9fe; cc_entrypoint=cli",
		"cch":               "x-anthropic-billing-header: cc_version=2.1.308.9fe; cc_entrypoint=cli; cch=00000;",
		"cc_workload":       "x-anthropic-billing-header: cc_version=2.1.308.9fe; cc_entrypoint=cli; cc_workload=x;",
		"one field":         "x-anthropic-billing-header: cc_version=2.1.308.9fe;",
		"order":             "x-anthropic-billing-header: cc_entrypoint=cli; cc_version=2.1.308.9fe;",
		"second not ep":     "x-anthropic-billing-header: cc_version=2.1.308.9fe; cc_other=cli;",
		"no dot":            "x-anthropic-billing-header: cc_version=9fe; cc_entrypoint=cli;",
		"upper fingerprint": "x-anthropic-billing-header: cc_version=2.1.308.9FE; cc_entrypoint=cli;",
		"long fingerprint":  "x-anthropic-billing-header: cc_version=2.1.308.9fe0; cc_entrypoint=cli;",
		"double space":      "x-anthropic-billing-header: cc_version=2.1.308.9fe;  cc_entrypoint=cli;",
	}
	for name, text := range bad {
		if _, err := ParseBillingHeader(text); err == nil {
			t.Errorf("%s: ParseBillingHeader(%q) accepted", name, text)
		}
	}
}

func TestUserID(t *testing.T) {
	device := strings.Repeat("ab", 32)
	session := "ce90e450-43fc-4765-8110-6aed6d650ca1"
	account := "0b7c2f3e-1d2a-4b5c-9d8e-7f6a5b4c3d2e"
	got := UserID(device, "", session)
	want := `{"device_id":"` + device + `","account_uuid":"","session_id":"` + session + `"}`
	if got != want {
		t.Fatalf("UserID = %s, want %s", got, want)
	}
	if d, a, s, err := ParseUserID(UserID(device, account, session)); err != nil || d != device || a != account || s != session {
		t.Fatalf("ParseUserID = %q %q %q %v", d, a, s, err)
	}
	bad := map[string]string{
		"not json":        `{`,
		"not object":      `[]`,
		"order":           `{"account_uuid":"","device_id":"` + device + `","session_id":"` + session + `"}`,
		"not string":      `{"device_id":"` + device + `","account_uuid":null,"session_id":"` + session + `"}`,
		"spaces":          `{"device_id": "` + device + `","account_uuid":"","session_id":"` + session + `"}`,
		"device 63 hex":   UserID(device[:63], "", session),
		"device upper":    UserID(strings.ToUpper(device), "", session),
		"account not ok":  UserID(device, "x", session),
		"session not v4":  UserID(device, "", "ce90e450-43fc-1765-8110-6aed6d650ca1"),
		"session variant": UserID(device, "", "ce90e450-43fc-4765-c110-6aed6d650ca1"),
	}
	for name, s := range bad {
		if _, _, _, err := ParseUserID(s); err == nil {
			t.Errorf("%s: ParseUserID(%s) accepted", name, s)
		}
	}
}

func TestUUID(t *testing.T) {
	ok := []string{
		"ce90e450-43fc-4765-8110-6aed6d650ca1", "ce90e450-43fc-4765-9110-6aed6d650ca1",
		"ce90e450-43fc-4765-a110-6aed6d650ca1", "ce90e450-43fc-4765-b110-6aed6d650ca1",
	}
	for _, s := range ok {
		if !IsUUIDv4(s) {
			t.Errorf("IsUUIDv4(%s) = false", s)
		}
	}
	bad := []string{
		"", "ce90e450-43fc-4765-8110-6aed6d650ca", "ce90e450x43fc-4765-8110-6aed6d650ca1",
		"CE90E450-43fc-4765-8110-6aed6d650ca1", "ce90e450-43fc-4765-8110-6aed6d650cg1",
	}
	for _, s := range bad {
		if IsUUID(s) {
			t.Errorf("IsUUID(%s) = true", s)
		}
	}
}
