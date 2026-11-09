package msgcat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A message is either plain text or a template written the way fmt.Sprintf takes
// it. Only four verbs are understood, which is all the server's messages use:
//
//	%s %v  any text           %d  a whole number          %q  quoted text
//
// %w (fmt.Errorf's wrapped error) reads as %s; a translation spells it %s.
//
// plus %% for a literal percent sign and an explicit argument index (%[2]s) so a
// translation can put its values in a different order from the English.

// verb is one formatting directive of a template.
type verb struct {
	index int  // 1-based argument position
	kind  byte // 's', 'd' or 'q'; 'v' is folded into 's'
}

// piece is a literal run or a verb, in template order.
type piece struct {
	text string
	verb *verb
}

// parse splits a template into pieces. It reports false for a directive it does
// not understand (an unsupported verb, a flag, a malformed index) so the caller
// can treat the message as plain text or reject it.
func parse(tmpl string) (pieces []piece, ok bool) {
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			pieces = append(pieces, piece{text: lit.String()})
			lit.Reset()
		}
	}
	next := 1
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c != '%' {
			lit.WriteByte(c)
			continue
		}
		i++
		if i >= len(tmpl) {
			return nil, false
		}
		if tmpl[i] == '%' {
			lit.WriteByte('%')
			continue
		}
		index := next
		if tmpl[i] == '[' {
			end := strings.IndexByte(tmpl[i:], ']')
			if end < 0 {
				return nil, false
			}
			n, err := strconv.Atoi(tmpl[i+1 : i+end])
			if err != nil || n < 1 {
				return nil, false
			}
			index = n
			i += end + 1
			if i >= len(tmpl) {
				return nil, false
			}
		}
		var kind byte
		switch tmpl[i] {
		case 's', 'v', 'w':
			kind = 's'
		case 'd':
			kind = 'd'
		case 'q':
			kind = 'q'
		default:
			return nil, false
		}
		flush()
		pieces = append(pieces, piece{verb: &verb{index: index, kind: kind}})
		next = index + 1
	}
	flush()
	return pieces, true
}

// verbsOf returns the directives of a template in argument order, and false when the
// template cannot be parsed. Two templates are interchangeable for translation when
// their verbs are equal.
func verbsOf(tmpl string) ([]verb, bool) {
	pieces, ok := parse(tmpl)
	if !ok {
		return nil, false
	}
	var vs []verb
	for _, p := range pieces {
		if p.verb != nil {
			vs = append(vs, *p.verb)
		}
	}
	// Order by argument position so "%[2]s %[1]s" compares equal to "%s %s" with the
	// same kinds at the same positions.
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j-1].index > vs[j].index; j-- {
			vs[j-1], vs[j] = vs[j], vs[j-1]
		}
	}
	return vs, true
}

// SameVerbs reports whether a translation takes exactly the arguments its source
// message takes: the same count, in the same kinds, however it orders them.
func SameVerbs(source, translation string) bool {
	a, okA := verbsOf(source)
	b, okB := verbsOf(translation)
	if !okA || !okB || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// matcher recognises messages produced from one template and recovers the values
// that were substituted into it.
type matcher struct {
	source string
	re     *regexp.Regexp
	kinds  []byte // kind of each capture group, in order of appearance
	index  []int  // argument position each capture group fills
	weight int    // literal characters in the template; longer means more specific
}

func newMatcher(source string) (*matcher, bool) {
	pieces, ok := parse(source)
	if !ok {
		return nil, false
	}
	m := &matcher{source: source}
	var re strings.Builder
	re.WriteString(`(?s)^`)
	for i, p := range pieces {
		if p.verb == nil {
			re.WriteString(regexp.QuoteMeta(p.text))
			m.weight += len(p.text)
			continue
		}
		last := i == len(pieces)-1
		switch p.verb.kind {
		case 'd':
			re.WriteString(`(-?\d+)`)
		case 'q':
			re.WriteString(`("(?:[^"\\]|\\.)*")`)
		default:
			if last {
				re.WriteString(`(.*)`)
			} else {
				re.WriteString(`(.+?)`)
			}
		}
		m.kinds = append(m.kinds, p.verb.kind)
		m.index = append(m.index, p.verb.index)
	}
	re.WriteString(`$`)
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return nil, false
	}
	m.re = compiled
	return m, true
}

// match returns the arguments, ordered by argument position, when msg was produced
// from the template.
func (m *matcher) match(msg string) ([]any, bool) {
	groups := m.re.FindStringSubmatch(msg)
	if groups == nil {
		return nil, false
	}
	args := make([]any, len(m.kinds))
	for i, kind := range m.kinds {
		raw := groups[i+1]
		var value any
		switch kind {
		case 'd':
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, false
			}
			value = n
		case 'q':
			s, err := strconv.Unquote(raw)
			if err != nil {
				return nil, false
			}
			value = s
		default:
			value = raw
		}
		pos := m.index[i] - 1
		if pos < 0 || pos >= len(args) {
			return nil, false
		}
		args[pos] = value
	}
	return args, true
}

// render fills a translated template with the recovered arguments.
func render(tmpl string, args []any) string {
	return fmt.Sprintf(tmpl, args...)
}
