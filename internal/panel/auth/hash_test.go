package auth

import (
	"errors"
	"regexp"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

var _ store.Hasher = Argon2Hasher{}

func TestHashDefaultParamsPHC(t *testing.T) {
	h, err := Argon2Hasher{}.Hash("s3nha forte")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^\$argon2id\$v=19\$m=65536,t=3,p=2\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	if !re.MatchString(h) {
		t.Fatalf("formato PHC inesperado: %s", h)
	}
	ok, err := Verify("s3nha forte", h)
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
	if ok, err := Verify("errada", h); err != nil || ok {
		t.Fatalf("senha errada aceita: %v, %v", ok, err)
	}
	h2, _ := Argon2Hasher{}.Hash("s3nha forte")
	if h2 == h {
		t.Fatal("sal repetido")
	}
}

func TestVerifyUsesHashParams(t *testing.T) {
	h, err := fastHasher.Hash("x")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=64,t=1,p=1\$`).MatchString(h) {
		t.Fatalf("parâmetros não gravados: %s", h)
	}
	if ok, err := Verify("x", h); err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
}

func TestVerifyInvalidHash(t *testing.T) {
	salt, key := "c2FsdHNhbHRzYWx0", "aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g" // 12 and 32 bytes
	for _, h := range []string{
		"",
		"plain",
		"$argon2i$v=19$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=18$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=x,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1,x=2$" + salt + "$" + key,
		"$argon2id$v=19$m=064,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=0,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=65,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=0$" + salt + "$" + key,
		"$argon2id$v=19$m=4,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=999999999,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$!!$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdA$" + key, // salt of 4 bytes
		"$argon2id$v=19$m=64,t=1,p=1$" + salt + "$!!",
		"$argon2id$v=19$m=64,t=1,p=1$" + salt + "$aGFzaA", // key of 4 bytes
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",    // example of spec 016
	} {
		if ok, err := Verify("x", h); ok || !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Verify(%q) = %v, %v; want ErrInvalidHash", h, ok, err)
		}
	}
}

func TestHashRandFailure(t *testing.T) {
	failRand(t, 0)
	if _, err := fastHasher.Hash("x"); err == nil {
		t.Fatal("esperado erro do sal")
	}
}
