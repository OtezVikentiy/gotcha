package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Fatalf("not a PHC argon2id string: %s", encoded)
	}
	ok, err := VerifyPassword("correct horse battery staple", encoded)
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong password", encoded)
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of same password identical: salt is not random")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2id$v=19$m=65536,t=1,p=4$only-salt",
		"$argon2id$v=19$m=65536,t=0,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=1,p=0$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=4294967295,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=65536,t=1,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=1,p=4$c2FsdHNhbHRzYWx0c2FsdA$",
		"$argon2id$v=19$m=65536,t=1,p=4$$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA",
	} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("malformed %q: want error", bad)
		}
	}
}
