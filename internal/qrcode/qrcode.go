// Package qrcode is a self-contained QR Code encoder, so TOTP enrollment QR
// codes render locally instead of leaking the secret to an external QR image
// service. Deliberately narrow scope: byte mode at error-correction level M,
// implementing ISO/IEC 18004 with no external dependencies.
package qrcode

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// eccLevelM is the 2-bit format indicator for error-correction level M.
const eccLevelM = 0b00

// verInfo holds the per-version block geometry for level M (ISO/IEC 18004
// Table 9). totalCodewords is the full codeword capacity; ecPerBlock and
// numBlocks define the Reed-Solomon block split. Data codewords are the
// remainder, distributed across the blocks with the longer blocks last.
type verInfo struct {
	totalCodewords int
	ecPerBlock     int
	numBlocks      int
}

// vtabM is indexed by version (1..40); index 0 is unused.
var vtabM = [41]verInfo{
	{}, // 0 unused
	{26, 10, 1}, {44, 16, 1}, {70, 26, 1}, {100, 18, 2}, {134, 24, 2},
	{172, 16, 4}, {196, 18, 4}, {242, 22, 4}, {292, 22, 5}, {346, 26, 5},
	{404, 30, 5}, {466, 22, 8}, {532, 22, 9}, {581, 24, 9}, {655, 24, 10},
	{733, 28, 10}, {815, 28, 11}, {901, 26, 13}, {991, 26, 14}, {1085, 26, 16},
	{1156, 26, 17}, {1258, 28, 17}, {1364, 28, 18}, {1474, 28, 20}, {1588, 28, 21},
	{1706, 28, 23}, {1828, 28, 25}, {1921, 28, 26}, {2051, 28, 28}, {2185, 28, 29},
	{2323, 28, 31}, {2465, 28, 33}, {2611, 28, 35}, {2761, 28, 37}, {2876, 28, 38},
	{3034, 28, 40}, {3196, 28, 43}, {3362, 28, 45}, {3532, 28, 47}, {3706, 28, 49},
}

// alignPositions gives the alignment-pattern centre coordinates per version
// (ISO/IEC 18004 Annex E). Every pairwise combination is an alignment centre,
// except the three that collide with the finder patterns.
var alignPositions = [41][]int{
	{}, {}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}, {6, 30, 54}, {6, 32, 58}, {6, 34, 62},
	{6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74}, {6, 30, 54, 78}, {6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90},
	{6, 28, 50, 72, 94}, {6, 26, 50, 74, 98}, {6, 30, 54, 78, 102}, {6, 28, 54, 80, 106}, {6, 32, 58, 84, 110}, {6, 30, 58, 86, 114}, {6, 34, 62, 90, 118},
	{6, 26, 50, 74, 98, 122}, {6, 30, 54, 78, 102, 126}, {6, 26, 52, 78, 104, 130}, {6, 30, 56, 82, 108, 134}, {6, 34, 60, 86, 112, 138}, {6, 30, 58, 86, 114, 142}, {6, 34, 62, 90, 118, 146},
	{6, 30, 54, 78, 102, 126, 150}, {6, 24, 50, 76, 102, 128, 154}, {6, 28, 54, 80, 106, 132, 158}, {6, 32, 58, 84, 110, 136, 162}, {6, 26, 54, 82, 110, 138, 166}, {6, 30, 58, 86, 114, 142, 170},
}

// versionPattern holds the 18-bit BCH-coded version information for versions
// 7..40 (ISO/IEC 18004 Annex D). Lower versions carry no version block.
var versionPattern = [41]int{
	0, 0, 0, 0, 0, 0, 0,
	0x07c94, 0x085bc, 0x09a99, 0x0a4d3, 0x0bbf6, 0x0c762, 0x0d847, 0x0e60d, 0x0f928,
	0x10b78, 0x1145d, 0x12a17, 0x13532, 0x149a6, 0x15683, 0x168c9, 0x177ec, 0x18ec4, 0x191e1,
	0x1afab, 0x1b08e, 0x1cc1a, 0x1d33f, 0x1ed75, 0x1f250, 0x209d5, 0x216f0, 0x228ba, 0x2379f,
	0x24b0b, 0x2542e, 0x26a64, 0x27541, 0x28c69,
}

// GF(256) arithmetic and Reed-Solomon error correction

// gfMul multiplies two GF(256) elements modulo the QR primitive polynomial
// 0x11D (x^8 + x^4 + x^3 + x^2 + 1), using Russian-peasant multiplication so no
// log/exp tables are needed.
func gfMul(x, y byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11d)
		z ^= int((y>>uint(i))&1) * int(x)
	}
	return byte(z)
}

// rsDivisor returns the generator polynomial of the given degree, as its
// coefficients excluding the leading 1 (constant term last).
func rsDivisor(degree int) []byte {
	result := make([]byte, degree)
	result[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			result[j] = gfMul(result[j], root)
			if j+1 < degree {
				result[j] ^= result[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return result
}

// rsRemainder returns the Reed-Solomon error-correction codewords for data:
// the remainder of data·x^len(divisor) divided by the generator polynomial.
func rsRemainder(data, divisor []byte) []byte {
	result := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for i, coef := range divisor {
			result[i] ^= gfMul(coef, factor)
		}
	}
	return result
}

// Bit buffer

type bitBuffer []bool

func (b *bitBuffer) append(val uint, n int) {
	for i := n - 1; i >= 0; i-- {
		*b = append(*b, (val>>uint(i))&1 != 0)
	}
}

// Encoding

// chooseVersion picks the smallest version whose level-M data capacity holds a
// byte-mode payload of n bytes, accounting for the version-dependent character
// count field width.
func chooseVersion(n int) (int, error) {
	for v := 1; v <= 40; v++ {
		ccBits := 8
		if v >= 10 {
			ccBits = 16
		}
		dataCodewords := vtabM[v].totalCodewords - vtabM[v].ecPerBlock*vtabM[v].numBlocks
		needBits := 4 + ccBits + 8*n
		if needBits <= dataCodewords*8 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("qrcode: payload of %d bytes is too large for QR level M", n)
}

// encodeData builds the final interleaved codeword stream (data + error
// correction) for payload at the given version.
func encodeData(payload []byte, version int) []byte {
	info := vtabM[version]
	dataCodewords := info.totalCodewords - info.ecPerBlock*info.numBlocks

	ccBits := 8
	if version >= 10 {
		ccBits = 16
	}

	var bb bitBuffer
	bb.append(0b0100, 4) // byte-mode indicator
	bb.append(uint(len(payload)), ccBits)
	for _, c := range payload {
		bb.append(uint(c), 8)
	}

	capacityBits := dataCodewords * 8
	// Terminator: up to four zero bits, not exceeding capacity.
	if term := capacityBits - len(bb); term > 4 {
		term = 4
		bb.append(0, term)
	} else {
		bb.append(0, term)
	}
	// Pad to a byte boundary, then alternate pad bytes 0xEC / 0x11.
	if rem := len(bb) % 8; rem != 0 {
		bb.append(0, 8-rem)
	}
	for pad := byte(0xEC); len(bb) < capacityBits; pad ^= 0xEC ^ 0x11 {
		bb.append(uint(pad), 8)
	}

	// Pack bits into data codewords.
	data := make([]byte, dataCodewords)
	for i := 0; i < len(bb); i++ {
		if bb[i] {
			data[i>>3] |= 1 << uint(7-(i&7))
		}
	}

	// Split into Reed-Solomon blocks. The last (totalData % numBlocks) blocks
	// each carry one extra data codeword.
	shortLen := dataCodewords / info.numBlocks
	numLong := dataCodewords % info.numBlocks
	divisor := rsDivisor(info.ecPerBlock)

	dataBlocks := make([][]byte, info.numBlocks)
	ecBlocks := make([][]byte, info.numBlocks)
	off := 0
	for i := 0; i < info.numBlocks; i++ {
		n := shortLen
		if i >= info.numBlocks-numLong {
			n++
		}
		dataBlocks[i] = data[off : off+n]
		ecBlocks[i] = rsRemainder(dataBlocks[i], divisor)
		off += n
	}

	// Interleave: column-wise across data blocks (skipping exhausted short
	// blocks), then column-wise across error-correction blocks.
	result := make([]byte, 0, info.totalCodewords)
	maxData := shortLen
	if numLong > 0 {
		maxData++
	}
	for col := 0; col < maxData; col++ {
		for i := 0; i < info.numBlocks; i++ {
			if col < len(dataBlocks[i]) {
				result = append(result, dataBlocks[i][col])
			}
		}
	}
	for col := 0; col < info.ecPerBlock; col++ {
		for i := 0; i < info.numBlocks; i++ {
			result = append(result, ecBlocks[i][col])
		}
	}
	return result
}

// Matrix construction

type matrix struct {
	size     int
	modules  [][]bool // true = black
	function [][]bool // true = reserved (function pattern; never holds data, never masked)
}

func newMatrix(version int) *matrix {
	size := version*4 + 17
	m := &matrix{size: size, modules: make([][]bool, size), function: make([][]bool, size)}
	for i := range m.modules {
		m.modules[i] = make([]bool, size)
		m.function[i] = make([]bool, size)
	}
	return m
}

func (m *matrix) set(r, c int, black bool) {
	m.modules[r][c] = black
	m.function[r][c] = true
}

// drawFunctionPatterns lays down everything that is fixed for a version:
// finders, separators, timing, alignment, the dark module, and the reserved
// format/version areas. Format and version bits themselves are drawn later.
func (m *matrix) drawFunctionPatterns(version int) {
	size := m.size

	// Timing patterns (row 6 and column 6).
	for i := 0; i < size; i++ {
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}

	// Finder patterns plus separators, at the three corners.
	m.drawFinder(0, 0)
	m.drawFinder(0, size-7)
	m.drawFinder(size-7, 0)

	// Alignment patterns at every centre combination, excluding the three that
	// overlap the finder patterns.
	pos := alignPositions[version]
	for _, r := range pos {
		for _, c := range pos {
			if (r == 6 && c == 6) || (r == 6 && c == size-7) || (r == size-7 && c == 6) {
				continue
			}
			m.drawAlignment(r, c)
		}
	}

	// Reserve the format-information area (drawn per-mask later).
	for i := 0; i <= 8; i++ {
		m.function[8][i] = true
		m.function[i][8] = true
	}
	for i := 0; i < 8; i++ {
		m.function[8][size-1-i] = true
		m.function[size-1-i][8] = true
	}
	// Dark module.
	m.set(size-8, 8, true)

	// Reserve and draw the version-information blocks (version >= 7).
	if version >= 7 {
		m.drawVersion(version)
	}
}

func (m *matrix) drawFinder(r, c int) {
	for dr := -1; dr <= 7; dr++ {
		for dc := -1; dc <= 7; dc++ {
			rr, cc := r+dr, c+dc
			if rr < 0 || rr >= m.size || cc < 0 || cc >= m.size {
				continue
			}
			// 7x7 ring with a 3x3 centre; everything else (the separator) white.
			black := dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6 &&
				(dr == 0 || dr == 6 || dc == 0 || dc == 6 ||
					(dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4))
			m.set(rr, cc, black)
		}
	}
}

func (m *matrix) drawAlignment(r, c int) {
	for dr := -2; dr <= 2; dr++ {
		for dc := -2; dc <= 2; dc++ {
			// 5x5 ring with a single centre dot.
			black := dr == -2 || dr == 2 || dc == -2 || dc == 2 || (dr == 0 && dc == 0)
			m.set(r+dr, c+dc, black)
		}
	}
}

func (m *matrix) drawVersion(version int) {
	v := versionPattern[version]
	size := m.size
	for i := 0; i < 18; i++ {
		bit := (v>>uint(i))&1 != 0
		a, b := i/3, i%3
		// Bottom-left block and its mirror in the top-right.
		m.set(size-11+b, a, bit)
		m.set(a, size-11+b, bit)
	}
}

// drawFormat writes the 15-bit BCH-coded format information for the given mask.
func (m *matrix) drawFormat(mask int) {
	size := m.size
	fb := (eccLevelM << 13) | (mask << 10)
	rem := fb
	for i := 14; i >= 10; i-- {
		if rem&(1<<uint(i)) != 0 {
			rem ^= 0x537 << uint(i-10)
		}
	}
	format := (fb | rem) ^ 0x5412

	for i := 0; i < 15; i++ {
		bit := (format>>uint(i))&1 != 0
		// Copy 1: wraps around the top-left finder.
		var r1, c1 int
		switch {
		case i < 6:
			r1, c1 = i, 8
		case i < 8:
			r1, c1 = i+1, 8
		case i == 8:
			r1, c1 = 8, 7
		default:
			r1, c1 = 8, 14-i
		}
		m.modules[r1][c1] = bit
		// Copy 2: split between bottom-left and top-right.
		if i < 8 {
			m.modules[8][size-1-i] = bit
		} else {
			m.modules[size-1-(14-i)][8] = bit
		}
	}
}

// drawCodewords lays the interleaved codeword stream into the data region using
// the standard two-column zigzag, skipping the vertical timing column. Modules
// left over after the data is exhausted (remainder bits) stay white.
func (m *matrix) drawCodewords(data []byte) {
	size := m.size
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := ((right + 1) & 2) == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if !m.function[y][x] && i < len(data)*8 {
					m.modules[y][x] = (data[i>>3]>>uint(7-(i&7)))&1 != 0
					i++
				}
			}
		}
	}
}

// maskCondition reports whether module (r, c) is inverted by the given mask
// (ISO/IEC 18004 Table 10).
func maskCondition(mask, r, c int) bool {
	switch mask {
	case 0:
		return (r+c)%2 == 0
	case 1:
		return r%2 == 0
	case 2:
		return c%3 == 0
	case 3:
		return (r+c)%3 == 0
	case 4:
		return (r/2+c/3)%2 == 0
	case 5:
		return (r*c)%2+(r*c)%3 == 0
	case 6:
		return ((r*c)%2+(r*c)%3)%2 == 0
	default:
		return ((r+c)%2+(r*c)%3)%2 == 0
	}
}

func (m *matrix) applyMask(mask int) {
	for r := 0; r < m.size; r++ {
		for c := 0; c < m.size; c++ {
			if !m.function[r][c] && maskCondition(mask, r, c) {
				m.modules[r][c] = !m.modules[r][c]
			}
		}
	}
}

// penalty scores a finished matrix by the four ISO/IEC 18004 rules; lower is
// better. Mask selection only affects appearance, not validity, so exactness
// here is cosmetic.
func (m *matrix) penalty() int {
	size := m.size
	score := 0

	// Rule 1: runs of five or more same-colour modules in a row/column.
	runScore := func(get func(i, j int) bool) {
		for i := 0; i < size; i++ {
			run := 1
			for j := 1; j < size; j++ {
				if get(i, j) == get(i, j-1) {
					run++
				} else {
					if run >= 5 {
						score += 3 + (run - 5)
					}
					run = 1
				}
			}
			if run >= 5 {
				score += 3 + (run - 5)
			}
		}
	}
	runScore(func(i, j int) bool { return m.modules[i][j] })
	runScore(func(i, j int) bool { return m.modules[j][i] })

	// Rule 2: 2x2 blocks of a single colour.
	for r := 0; r < size-1; r++ {
		for c := 0; c < size-1; c++ {
			v := m.modules[r][c]
			if m.modules[r][c+1] == v && m.modules[r+1][c] == v && m.modules[r+1][c+1] == v {
				score += 3
			}
		}
	}

	// Rule 3: 1:1:3:1:1 finder-like patterns (with a 4-module light run on one
	// side) in any row or column.
	p1 := []bool{true, false, true, true, true, false, true, false, false, false, false}
	p2 := []bool{false, false, false, false, true, false, true, true, true, false, true}
	match := func(get func(k int) bool, start int) bool {
		for off, pat := range [][]bool{p1, p2} {
			_ = off
			ok := true
			for k := 0; k < 11; k++ {
				if get(start+k) != pat[k] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	for i := 0; i < size; i++ {
		for j := 0; j <= size-11; j++ {
			row := i
			if match(func(k int) bool { return m.modules[row][k] }, j) {
				score += 40
			}
			col := i
			if match(func(k int) bool { return m.modules[k][col] }, j) {
				score += 40
			}
		}
	}

	// Rule 4: deviation of the dark-module proportion from 50%.
	dark := 0
	for r := 0; r < size; r++ {
		for c := 0; c < size; c++ {
			if m.modules[r][c] {
				dark++
			}
		}
	}
	total := size * size
	percent := dark * 100 / total
	dev := percent - 50
	if dev < 0 {
		dev = -dev
	}
	score += (dev / 5) * 10

	return score
}

// clone returns a deep copy of the matrix.
func (m *matrix) clone() *matrix {
	c := &matrix{size: m.size, modules: make([][]bool, m.size), function: make([][]bool, m.size)}
	for i := range m.modules {
		c.modules[i] = append([]bool(nil), m.modules[i]...)
		c.function[i] = append([]bool(nil), m.function[i]...)
	}
	return c
}

// Encode returns the QR module grid for text (true = black), using byte mode at
// error-correction level M, with the lowest-penalty mask applied.
func Encode(text string) ([][]bool, error) {
	payload := []byte(text)
	version, err := chooseVersion(len(payload))
	if err != nil {
		return nil, err
	}

	codewords := encodeData(payload, version)

	base := newMatrix(version)
	base.drawFunctionPatterns(version)
	base.drawCodewords(codewords)

	var best *matrix
	bestScore := -1
	for mask := 0; mask < 8; mask++ {
		cand := base.clone()
		cand.applyMask(mask)
		cand.drawFormat(mask)
		if s := cand.penalty(); best == nil || s < bestScore {
			best, bestScore = cand, s
		}
	}
	return best.modules, nil
}

// SVG rendering

const quietZone = 4 // modules of mandatory light margin

// SVG renders text as a self-contained SVG QR code. The viewBox is in module
// units (1 unit per module) so the image scales crisply to any pixel size.
func SVG(text string) (string, error) {
	grid, err := Encode(text)
	if err != nil {
		return "", err
	}
	n := len(grid)
	dim := n + 2*quietZone

	var path strings.Builder
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			if grid[r][c] {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", c+quietZone, r+quietZone)
			}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, dim, dim)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="#ffffff"/>`, dim, dim)
	fmt.Fprintf(&sb, `<path d="%s" fill="#000000"/>`, path.String())
	sb.WriteString(`</svg>`)
	return sb.String(), nil
}

// DataURI renders text as a base64-encoded SVG data URI, suitable for an <img>
// src attribute. Returns an empty string on error.
func DataURI(text string) string {
	svg, err := SVG(text)
	if err != nil {
		return ""
	}
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}
