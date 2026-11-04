package admin

import (
	"encoding/json"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Task 364: a deployment (Azure, ADR 0065) is an entity kind of the admin
// API: it can be seen, banned and unbanned like the others.
func TestAdminDeploymentEntity(t *testing.T) {
	s := newServer(t, fastCfg)
	want(t, do(t, s, "POST", "/admin/entities/deployment/acme-gpt-x/ban", "application/json", `{"reason":"x"}`), 200)
	r := do(t, s, "GET", "/admin/entities/deployment/acme-gpt-x", "", "")
	want(t, r, 200)
	var st limits.EntityState
	if err := json.Unmarshal([]byte(r.body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Entity.Kind != limits.KindDeployment || st.Status != limits.StatusBanned {
		t.Fatalf("state = %+v", st)
	}
	want(t, do(t, s, "POST", "/admin/entities/deployment/acme-gpt-x/unban", "", ""), 200)
}
