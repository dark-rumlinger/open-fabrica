package security

import "golang.org/x/crypto/bcrypt"

// bcryptCost is the work factor for password hashing. 12 ~= ~250ms per hash on
// current hardware - slow enough to blunt offline GPU cracking of a leaked hash,
// fast enough to stay responsive at login. The cost is embedded in each hash, so
// raising it does not invalidate passwords hashed at a lower cost.
const bcryptCost = 12

// dummyHash is a valid bcrypt hash (cost bcryptCost) of a throwaway value,
// generated at package load rather than hardcoded so it always parses and uses
// the current cost, and DummyPasswordCompare does real work against it.
var dummyHash = mustDummyHash()

// mustDummyHash panics on a generation failure: a bad hash would silently
// disable login-timing equalization, so it must never boot.
func mustDummyHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("of-timing-equalizer-not-a-real-password"), bcryptCost)
	if err != nil {
		panic("security: dummy bcrypt hash generation failed: " + err.Error())
	}
	return h
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(bytes), err
}

func CheckPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// DummyPasswordCompare performs a bcrypt comparison against a fixed hash and
// discards the result. Call it on the "no such user" branch of a login handler
// so response timing does not reveal whether the username exists.
func DummyPasswordCompare() {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte("not-the-password"))
}
