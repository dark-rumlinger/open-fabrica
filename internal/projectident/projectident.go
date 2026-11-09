// Package projectident mints the friendly name and domain (for example
// "Crimson Otter" and crimson-otter-x7k2p.localhost) that a new project gets when
// its creator does not choose one. Shared by the admin API and the bot so both
// create projects the same way.
package projectident

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"openfabrica/db/models"
)

// Adjective-noun word lists used to mint friendly project identities like
// "Crimson Otter" -> crimson-otter.localhost. Kept short, lowercase, ASCII and
// brandable so both the display name and the domain slug read cleanly.
var nameAdjectives = []string{
	"amber", "azure", "brave", "bright", "calm", "clever", "cobalt", "coral",
	"cosmic", "crimson", "crystal", "dapper", "electric", "ember", "fancy",
	"fleet", "frosty", "gentle", "gilded", "golden", "hidden", "ivory", "jade",
	"jolly", "lucky", "lunar", "mellow", "merry", "misty", "noble", "polar",
	"quiet", "rapid", "royal", "ruby", "rustic", "sage", "scarlet", "silent",
	"silver", "snowy", "solar", "stellar", "sunny", "swift", "tidal", "velvet",
	"vivid", "wild",
}

var nameNouns = []string{
	"otter", "falcon", "harbor", "meadow", "comet", "willow", "lantern",
	"river", "summit", "canyon", "grove", "beacon", "cedar", "badger", "heron",
	"maple", "orchard", "pebble", "quartz", "raven", "sparrow", "thistle",
	"vortex", "walrus", "yarrow", "fox", "lynx", "marten", "osprey", "panther",
	"quokka", "robin", "sable", "tern", "urchin", "viper", "wren", "bison",
	"crane", "dolphin", "eagle", "ferret", "gecko", "hawk", "ibis", "jackal",
	"koala",
}

// domainSuffix is appended to generated slugs. Matches the placeholder the UI
// shows for manual entry (example.localhost).
const domainSuffix = ".localhost"

// slugTokenAlphabet and slugTokenLen control the random token appended to every
// generated domain slug. The token keeps the slug unique across instances (an
// imported project that also minted "crimson-otter" still gets a distinct
// domain); 36^5 (~60M) tokens per adjective-noun pair makes collisions
// vanishingly unlikely. Lowercase alphanumeric keeps the token DNS-label safe.
const slugTokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
const slugTokenLen = 5

// randomSlugToken returns a short lowercase alphanumeric token for embedding in
// a domain slug. Never empty, DNS-label safe.
func randomSlugToken() string {
	b := make([]byte, slugTokenLen)
	for i := range b {
		b[i] = slugTokenAlphabet[rand.Intn(len(slugTokenAlphabet))]
	}
	return string(b)
}

// Generate returns a friendly project name and matching domain
// derived from the same adjective-noun pair, the domain carrying a random token
// (see randomSlugToken). Both are checked against existing projects; after a
// run of random misses it falls back to numbered name suffixes so the loop
// terminates even when the (tokenless) name space is crowded.
func Generate(db *sql.DB) (name, domain string, err error) {
	const maxRandomTries = 40
	for i := 0; i < maxRandomTries; i++ {
		adj := nameAdjectives[rand.Intn(len(nameAdjectives))]
		noun := nameNouns[rand.Intn(len(nameNouns))]
		name = capitalize(adj) + " " + capitalize(noun)
		domain = adj + "-" + noun + "-" + randomSlugToken() + domainSuffix
		free, ferr := identityFree(db, name, domain)
		if ferr != nil {
			return "", "", ferr
		}
		if free {
			return name, domain, nil
		}
	}

	// Crowded name space: anchor on one pair and walk numbered suffixes. The
	// domain still gets a token so it stays globally unique even here.
	adj := nameAdjectives[rand.Intn(len(nameAdjectives))]
	noun := nameNouns[rand.Intn(len(nameNouns))]
	baseName := capitalize(adj) + " " + capitalize(noun)
	baseSlug := adj + "-" + noun
	for n := 2; ; n++ {
		name = fmt.Sprintf("%s %d", baseName, n)
		domain = fmt.Sprintf("%s-%d-%s%s", baseSlug, n, randomSlugToken(), domainSuffix)
		free, ferr := identityFree(db, name, domain)
		if ferr != nil {
			return "", "", ferr
		}
		if free {
			return name, domain, nil
		}
	}
}

// identityFree reports whether both the name and domain are unused. Name
// uniqueness is enforced here (the column has no DB constraint); domain has a
// UNIQUE index which acts as the ultimate backstop at insert time.
func identityFree(db *sql.DB, name, domain string) (bool, error) {
	nameTaken, err := models.ProjectNameExists(db, name)
	if err != nil {
		return false, err
	}
	if nameTaken {
		return false, nil
	}
	if _, err := models.GetProjectByDomain(db, domain); err == nil {
		return false, nil // a project already owns this domain
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return true, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
