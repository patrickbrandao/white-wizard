package config

import "testing"

func TestApplyEnvStoreKind(t *testing.T) {
	cases := []struct {
		name, store, addr, want string
		wantAddr                bool
	}{
		{"addr implies valkey", "", "v1:6379,v2:6379", "valkey", true},
		{"explicit memory wins over addr", "memory", "v1:6379", "memory", true},
		{"explicit valkey without addr", "valkey", "", "valkey", false},
		{"nothing set keeps yaml kind", "", "", "memory", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WWZ_STORE", tc.store)
			t.Setenv("WWZ_VALKEY_ADDR", tc.addr)
			c := &Config{}
			c.Store.Kind = "memory"
			ApplyEnv(c)
			if c.Store.Kind != tc.want {
				t.Fatalf("kind = %q, want %q", c.Store.Kind, tc.want)
			}
			if tc.wantAddr && (len(c.Store.Addr) == 0 || c.Store.Addr[0] != "v1:6379") {
				t.Fatalf("addr = %v", c.Store.Addr)
			}
			if !tc.wantAddr && len(c.Store.Addr) != 0 {
				t.Fatalf("addr = %v, want empty", c.Store.Addr)
			}
		})
	}
}
