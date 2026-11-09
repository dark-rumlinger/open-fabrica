package store

import (
	"database/sql"
	"sort"
	"strings"

	"openfabrica/internal/dbh"
)

// SuggestArtifactPaths returns up to `limit` existing artifact paths that most
// closely resemble missingPath, best match first - the "did-you-mean" data behind
// not-found recovery hints. kind scopes the candidate set ("" for every kind).
// Ranking favors shared basename or stem, then edit distance; case-insensitive.
// Best-effort: a query error yields no suggestions rather than a spurious one.
func SuggestArtifactPaths(db dbh.Handle, kind, missingPath string, limit int) []string {
	if db == nil || strings.TrimSpace(missingPath) == "" {
		return nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if kind != "" {
		rows, err = db.Query(`SELECT path FROM of_files WHERE kind = ? AND is_deleted = 0`, kind)
	} else {
		rows, err = db.Query(`SELECT DISTINCT path FROM of_files WHERE is_deleted = 0`)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()

	targetBase := strings.ToLower(pathBaseName(missingPath))
	targetStem := stem(targetBase)
	// A candidate qualifies only when its FILENAME is close to the guessed one:
	// same basename (right file, wrong directory), same stem (glide vs
	// glide.core), or a small typo. Distance is measured on basenames so the
	// directory prefix never dilutes it, and unrelated files never leak in.
	typoBudget := len(targetBase) / 3
	if typoBudget < 2 {
		typoBudget = 2
	}

	type cand struct {
		path  string
		score int
	}
	var cands []cand
	for rows.Next() {
		var p string
		if rows.Scan(&p) != nil {
			continue
		}
		if strings.EqualFold(p, missingPath) {
			continue // an exact match is not a "did you mean" (and shouldn't occur on not-found)
		}
		base := strings.ToLower(pathBaseName(p))
		dist := levenshteinDist(targetBase, base)
		switch {
		case base == targetBase:
			cands = append(cands, cand{p, -100}) // same filename, different dir: almost certainly the one
		case stem(base) == targetStem && targetStem != "":
			cands = append(cands, cand{p, -40 + dist}) // same family, e.g. glide.core / glide.state
		case dist <= typoBudget:
			cands = append(cands, cand{p, dist}) // a small typo away
		}
	}
	_ = rows.Err() // best-effort suggestions; tolerated like the query error
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return cands[i].path < cands[j].path
	})

	if limit <= 0 {
		limit = 3
	}
	out := make([]string, 0, limit)
	for _, c := range cands {
		out = append(out, c.path)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// SuggestArtifactPathsHint formats SuggestArtifactPaths as a trailing
// " Did you mean: a, b?" clause, or "" when nothing is close enough. Callers
// append it directly to a not-found error message.
func SuggestArtifactPathsHint(db dbh.Handle, kind, missingPath string) string {
	s := SuggestArtifactPaths(db, kind, missingPath, 3)
	if len(s) == 0 {
		return ""
	}
	return " Did you mean: " + strings.Join(s, ", ") + "?"
}

// ResolveUniqueBasename returns the one stored artifact path whose FILENAME
// equals missingPath's filename (case-insensitive), ONLY when exactly one such
// path exists and it differs from missingPath - the auto-recovery counterpart to
// the did-you-mean hint (a not-found is nearly always the right file in the wrong
// directory). Returns ("", false) when ambiguous or on a query error, so it never guesses.
func ResolveUniqueBasename(db dbh.Handle, kind, missingPath string) (string, bool) {
	if db == nil || strings.TrimSpace(missingPath) == "" {
		return "", false
	}
	target := strings.ToLower(pathBaseName(missingPath))
	if target == "" {
		return "", false
	}
	var (
		rows *sql.Rows
		err  error
	)
	if kind != "" {
		rows, err = db.Query(`SELECT path FROM of_files WHERE kind = ? AND is_deleted = 0`, kind)
	} else {
		rows, err = db.Query(`SELECT DISTINCT path FROM of_files WHERE is_deleted = 0`)
	}
	if err != nil {
		return "", false
	}
	defer rows.Close()

	var match string
	count := 0
	for rows.Next() {
		var p string
		if rows.Scan(&p) != nil {
			continue
		}
		if strings.EqualFold(p, missingPath) {
			continue // the exact path was already looked up and missed
		}
		if strings.ToLower(pathBaseName(p)) == target {
			match = p
			count++
			if count > 1 {
				return "", false // basename not unique - ambiguous, do not guess
			}
		}
	}
	if rows.Err() != nil || count != 1 {
		return "", false
	}
	return match, true
}

// SuggestName returns up to `limit` names from candidates closest to target by
// case-insensitive edit distance, nearest first, within a small typo budget. The
// flat-name sibling of SuggestArtifactPaths, for did-you-mean over bare identifiers.
func SuggestName(target string, candidates []string, limit int) []string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return nil
	}
	budget := len(t) / 3
	if budget < 2 {
		budget = 2
	}
	type c struct {
		name string
		d    int
	}
	var cs []c
	for _, cand := range candidates {
		if strings.EqualFold(cand, target) {
			continue
		}
		if d := levenshteinDist(t, strings.ToLower(cand)); d <= budget {
			cs = append(cs, c{cand, d})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].name < cs[j].name
	})
	if limit <= 0 {
		limit = 3
	}
	out := make([]string, 0, limit)
	for _, x := range cs {
		out = append(out, x.name)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// pathBaseName returns the last '/'-separated segment of p (its filename).
func pathBaseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// stem returns the basename up to its first '.', so "glide.core.js" -> "glide".
// It collapses multi-part module names (foo.state.js, foo.render.js) to the
// shared family name that the model usually gets right.
func stem(base string) string {
	if i := strings.Index(base, "."); i >= 0 {
		return base[:i]
	}
	return base
}

// levenshteinDist is the classic edit distance (insert/delete/substitute), used
// only to rank a bounded candidate set. Self-contained so package store carries
// no dependency on the content package's copy.
func levenshteinDist(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
