package engine

import (
	"strings"
	"testing"
)

// TestPlanSecurityContractResolves guards the one contract reference made by a Go
// string literal (planSecurityContract -> contracts.Render("e2ee-security-model")):
// a renamed contract id compiles fine and would silently inject nothing at PLAN
// time. Assert an encryption brief still yields a non-empty model, and a plain
// brief yields none.
func TestPlanSecurityContractResolves(t *testing.T) {
	if got := planSecurityContract("an end-to-end encrypted private chat app"); strings.TrimSpace(got) == "" {
		t.Error("planSecurityContract returned empty for an encryption brief - the e2ee-security-model contract reference may have drifted")
	}
	if got := planSecurityContract("a public blog with posts and comments"); got != "" {
		t.Errorf("planSecurityContract must be empty for a non-encryption brief, got %q", got)
	}
}

// TestBriefImpliesEncryptionAccountVeto pins the account-shape veto: a brief
// whose "password" belongs to user ACCOUNTS (register/login, admin roles,
// per-account server data) must NOT get the E2EE security model appended -
// that injection contradicts the brief's own auth requirements. Account wording
// co-occurring with "chat" is not sufficient to imply E2EE. Explicit encryption
// wording still wins.
func TestBriefImpliesEncryptionAccountVeto(t *testing.T) {
	accountsApp := "A browser-based multiplayer sandbox app. Players register an account with username and password and log in; an admin role can kick users and reset the workspace; inventory is persistent per account. A chat overlay lets everyone talk."
	if briefImpliesEncryption(accountsApp) {
		t.Error("an accounts-shaped brief (register/login/admin) must not imply the E2EE model, even with password+chat co-occurring")
	}
	room := "A private watch-party room you join with a room name and password, with live chat."
	if !briefImpliesEncryption(room) {
		t.Error("a password-joined private room brief must still imply the E2EE model")
	}
	explicit := "An end-to-end encrypted messenger where users register accounts."
	if !briefImpliesEncryption(explicit) {
		t.Error("explicit encryption wording must always imply the model, even alongside accounts")
	}
}

// TestBriefImpliesEncryptionContentSecrecyPhrases pins the broadened matcher: a brief
// that states E2EE intent WITHOUT the literal "encrypt" ("only participants can read",
// "disappearing messages") implies the model, while the account veto still overrides
// those phrases (E2EE replaces server-side auth).
func TestBriefImpliesEncryptionContentSecrecyPhrases(t *testing.T) {
	for _, brief := range []string{
		"A group chat where a message is readable by only participants; nobody else can read it.",
		"A disappearing message app for private one-to-one conversations.",
	} {
		if !briefImpliesEncryption(brief) {
			t.Errorf("a content-secrecy brief should imply the E2EE model: %q", brief)
		}
	}
	vetoed := "A team messenger with user accounts and login where only participants can read a thread."
	if briefImpliesEncryption(vetoed) {
		t.Error("an accounts-shaped brief must not imply E2EE even with a content-secrecy phrase")
	}
}
