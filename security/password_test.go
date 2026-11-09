package security

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("HashPassword returned empty hash")
	}
	if hash == pw {
		t.Fatal("hash must not equal the plaintext password")
	}
	if !CheckPassword(pw, hash) {
		t.Error("CheckPassword rejected the correct password")
	}
}

func TestCheckPassword_WrongPasswordFails(t *testing.T) {
	hash, err := HashPassword("the-real-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if CheckPassword("not-the-password", hash) {
		t.Error("CheckPassword accepted an incorrect password")
	}
}

func TestCheckPassword_MalformedHashFails(t *testing.T) {
	if CheckPassword("anything", "not-a-valid-bcrypt-hash") {
		t.Error("CheckPassword accepted a malformed hash")
	}
}

func TestHashPassword_SaltMakesHashesDiffer(t *testing.T) {
	const pw = "same-password-twice"
	h1, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword h1: %v", err)
	}
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword h2: %v", err)
	}
	if h1 == h2 {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	// Despite differing, both must verify.
	if !CheckPassword(pw, h1) || !CheckPassword(pw, h2) {
		t.Error("a salted hash failed to verify its own password")
	}
}

func TestDummyPasswordCompare_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DummyPasswordCompare panicked: %v", r)
		}
	}()
	DummyPasswordCompare()
}
