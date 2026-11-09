package admin

import (
	"strings"
	"testing"
)

// The same vectors run against core/password.js (web/tests/password.test.js); the
// two must agree.
var passwordVectors = []struct {
	password string
	ok       bool
}{
	{"Abcdefg1!", true},
	{"Éclair12", true},
	{"Пароль12!", true},
	{"パスワードです1!", true},
	{"كلمةالسر١٢٣!", true},
	{"पासवर्ड१२३४!", true},
	{"あいうえおか1!", true},
	{"abcdefg1!", false},   // no capital
	{"Abcdefg!!", false},   // no digit
	{"Abcdefg12", false},   // no special
	{"Ab1!", false},        // short
	{"あいうえおか1", false},     // seven characters
	{"пароль12!", false},   // lowercase Cyrillic is not a capital
	{"ПАРОЛЬ", false},      // short
	{"1234567890!", false}, // no letter at all
}

func TestPasswordRulesAreAboutCharactersNotBytes(t *testing.T) {
	for _, v := range passwordVectors {
		err := validatePasswordComplexity(v.password)
		if (err == nil) != v.ok {
			t.Errorf("%q: got %v, want ok=%v", v.password, err, v.ok)
		}
	}
}

func TestUsernameLengthCountsCharacters(t *testing.T) {
	const pw = "Abcdefg1!"
	if err := validateSetupCredentials(strings.Repeat("あ", 64), pw); err != nil {
		t.Errorf("64 characters (192 bytes) must be accepted: %v", err)
	}
	if err := validateSetupCredentials(strings.Repeat("あ", 65), pw); err == nil {
		t.Error("65 characters must be rejected")
	}
	if err := validateSetupCredentials(strings.Repeat("a", 65), pw); err == nil {
		t.Error("65 ASCII characters must be rejected")
	}
}
