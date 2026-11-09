// Package doctemplate fills an HTML template with request data for the runtime
// PDF endpoint. It is deliberately tiny: values are HTML-escaped, there is no
// expression language and no way to reach anything but the data passed in, so a
// visitor-supplied value can never become markup or script.
//
//	{{name}}            a value; dotted paths walk nested objects ({{customer.name}})
//	{{#each items}}...{{/each}}   repeat the block for each element of an array
//	  inside a block: {{this}} is a scalar element, {{field}} reads the element's
//	  field, {{@index}} is the 1-based position, and {{../name}} reaches the
//	  enclosing scope
//
// A name that does not resolve renders as nothing.
package doctemplate

import (
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
)

const (
	// MaxOutputBytes caps the rendered document.
	MaxOutputBytes = 2 << 20
	// MaxEachItems caps one repeated block, so a large array cannot multiply a
	// template into something enormous.
	MaxEachItems = 1000
	maxDepth     = 4
)

var (
	// ErrTooLarge is returned when the rendered output would exceed MaxOutputBytes.
	ErrTooLarge = errors.New("rendered document is too large")
	errSyntax   = errors.New("template syntax error")
)

// scope is one level of data: the value in scope plus the enclosing scope.
type scope struct {
	value  any
	index  int
	parent *scope
}

// Render fills tmpl from data.
func Render(tmpl string, data map[string]any) (string, error) {
	if err := validate(tmpl); err != nil {
		return "", err
	}
	var b strings.Builder
	root := &scope{value: data}
	if err := render(&b, tmpl, root, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// validate checks the whole template up front, so a malformed or too deeply
// nested block is an error even when the data would never reach it.
func validate(tmpl string) error {
	depth := 0
	for {
		open := strings.Index(tmpl, "{{")
		if open < 0 {
			break
		}
		tmpl = tmpl[open+2:]
		end := strings.Index(tmpl, "}}")
		if end < 0 {
			return fmt.Errorf("%w: unclosed {{", errSyntax)
		}
		tag := strings.TrimSpace(tmpl[:end])
		tmpl = tmpl[end+2:]
		switch {
		case strings.HasPrefix(tag, "#each "):
			depth++
			if depth > maxDepth {
				return fmt.Errorf("%w: blocks nested deeper than %d", errSyntax, maxDepth)
			}
		case tag == "/each":
			depth--
			if depth < 0 {
				return fmt.Errorf("%w: {{/each}} without {{#each}}", errSyntax)
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("%w: {{#each}} is never closed", errSyntax)
	}
	return nil
}

func render(b *strings.Builder, tmpl string, sc *scope, depth int) error {
	for {
		open := strings.Index(tmpl, "{{")
		if open < 0 {
			b.WriteString(tmpl)
			return checkSize(b)
		}
		b.WriteString(tmpl[:open])
		tmpl = tmpl[open+2:]
		end := strings.Index(tmpl, "}}")
		if end < 0 {
			return fmt.Errorf("%w: unclosed {{", errSyntax)
		}
		tag := strings.TrimSpace(tmpl[:end])
		tmpl = tmpl[end+2:]

		if strings.HasPrefix(tag, "#each ") {
			if depth >= maxDepth {
				return fmt.Errorf("%w: blocks nested deeper than %d", errSyntax, maxDepth)
			}
			body, rest, err := splitBlock(tmpl)
			if err != nil {
				return err
			}
			tmpl = rest
			items, _ := lookup(sc, strings.TrimSpace(strings.TrimPrefix(tag, "#each "))).([]any)
			if len(items) > MaxEachItems {
				items = items[:MaxEachItems]
			}
			for i, item := range items {
				if err := render(b, body, &scope{value: item, index: i + 1, parent: sc}, depth+1); err != nil {
					return err
				}
			}
			continue
		}
		if tag == "/each" {
			return fmt.Errorf("%w: {{/each}} without {{#each}}", errSyntax)
		}
		b.WriteString(html.EscapeString(stringify(lookup(sc, tag))))
		if err := checkSize(b); err != nil {
			return err
		}
	}
}

// splitBlock returns the text up to the {{/each}} that closes the block just
// opened (honouring nested blocks) and the text after it.
func splitBlock(s string) (body, rest string, err error) {
	depth, pos := 1, 0
	for {
		i := strings.Index(s[pos:], "{{")
		if i < 0 {
			return "", "", fmt.Errorf("%w: {{#each}} is never closed", errSyntax)
		}
		i += pos
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return "", "", fmt.Errorf("%w: unclosed {{", errSyntax)
		}
		tag := strings.TrimSpace(s[i+2 : i+j])
		switch {
		case strings.HasPrefix(tag, "#each "):
			depth++
		case tag == "/each":
			depth--
			if depth == 0 {
				return s[:i], s[i+j+2:], nil
			}
		}
		pos = i + j + 2
	}
}

func checkSize(b *strings.Builder) error {
	if b.Len() > MaxOutputBytes {
		return ErrTooLarge
	}
	return nil
}

// lookup resolves a tag against the scope.
func lookup(sc *scope, tag string) any {
	for strings.HasPrefix(tag, "../") {
		tag = strings.TrimPrefix(tag, "../")
		if sc.parent != nil {
			sc = sc.parent
		}
	}
	switch tag {
	case "this", ".":
		return sc.value
	case "@index":
		return sc.index
	}
	cur := sc.value
	for _, part := range strings.Split(tag, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// stringify renders a JSON-decoded value as text.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case []any, map[string]any:
		return ""
	}
	return fmt.Sprint(v)
}
