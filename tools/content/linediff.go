package content

import "strings"

// Line-level diff primitives shared by the post-edit echo (edit_echo.go). They
// classify each line of a before/after body as kept, inserted, or deleted so the
// echo can mark exactly which lines an edit touched.

type diffKind int

const (
	diffEq  diffKind = iota // line present in both bodies
	diffIns                 // line only in the new body
	diffDel                 // line only in the old body
)

type diffOp struct {
	kind diffKind
	text string
}

// splitLinesKeep splits a body into lines on "\n" without keeping the
// separators, returning an empty slice for an empty body so callers can treat
// "no content" as zero lines rather than one empty line.
func splitLinesKeep(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lcsDiff returns the line operations transforming a into b, derived from a
// longest-common-subsequence table. Ops are emitted in output order: diffEq for
// a shared line, diffDel for a line only in a, diffIns for a line only in b.
//
// The shared head and tail are peeled off first (an edit usually touches a small
// middle), and a middle too large for the table is reported as one delete run and
// one insert run instead of allocating (n+1)*(m+1) cells.
func lcsDiff(a, b []string) []diffOp {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	ops := make([]diffOp, 0, len(a)+len(b)-head-tail)
	for _, s := range a[:head] {
		ops = append(ops, diffOp{kind: diffEq, text: s})
	}
	midA, midB := a[head:len(a)-tail], b[head:len(b)-tail]
	if (len(midA)+1)*(len(midB)+1) > maxDiffCells {
		for _, s := range midA {
			ops = append(ops, diffOp{kind: diffDel, text: s})
		}
		for _, s := range midB {
			ops = append(ops, diffOp{kind: diffIns, text: s})
		}
	} else {
		ops = append(ops, lcsDiffTable(midA, midB)...)
	}
	for _, s := range a[len(a)-tail:] {
		ops = append(ops, diffOp{kind: diffEq, text: s})
	}
	return ops
}

// maxDiffCells bounds the LCS table (about 2000 x 2000 changed lines).
const maxDiffCells = 4_000_000

func lcsDiffTable(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = length of the LCS of a[i:] and b[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: diffEq, text: a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{kind: diffDel, text: a[i]})
			i++
		default:
			ops = append(ops, diffOp{kind: diffIns, text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: diffDel, text: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: diffIns, text: b[j]})
	}
	return ops
}
