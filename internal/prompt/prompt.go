// Package prompt composes positive/negative prompts from reusable blocks.
package prompt

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Block is a reusable prompt fragment: a quality header, a character
// description, a LoRA trigger, a base negative...
type Block struct {
	ID   string   `json:"id"`
	Kind string   `json:"kind,omitempty"`
	Name string   `json:"name,omitempty"`
	Text string   `json:"text"`
	Tags []string `json:"tags,omitempty"`
}

// Preset is an ordered list of block ids.
type Preset struct {
	ID     string   `json:"id"`
	Name   string   `json:"name,omitempty"`
	Blocks []string `json:"blocks"`
}

var (
	varRe    = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	weightRe = regexp.MustCompile(`^\((.+):(\d*\.?\d+)\)$`)
)

// Render substitutes {{name}} placeholders. Every missing variable is
// reported at once.
func Render(text string, vars map[string]string) (string, error) {
	var missing []string
	seen := map[string]bool{}
	out := varRe.ReplaceAllStringFunc(text, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
			return m
		}
		return v
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("missing variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Compose renders each block, joins them and normalizes the result.
func Compose(blocks []Block, vars map[string]string) (string, error) {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		t, err := Render(b.Text, vars)
		if err != nil {
			return "", fmt.Errorf("block %q: %w", b.ID, err)
		}
		parts = append(parts, t)
	}
	return Normalize(strings.Join(parts, ", ")), nil
}

// Normalize tidies a comma separated tag prompt: collapses whitespace, drops
// empty entries, removes duplicates (case-insensitive, first one wins) and
// writes weights canonically, so (tag:1.0) becomes tag and (tag:1.10) becomes
// (tag:1.1). Commas inside (), [] and <> do not split. When a tag appears
// several times it keeps the position of its first occurrence and the
// strongest emphasis (the weight farthest from 1, the higher one on a tie),
// whatever the order: "cat, (cat:1.4)" and "(cat:1.4), cat" give the same tag.
func Normalize(s string) string {
	type entry struct {
		text string
		w    float64
	}
	idx := map[string]int{}
	var out []entry
	for _, part := range splitTop(s) {
		tag, key, w := normalizeTag(part)
		if tag == "" {
			continue
		}
		i, seen := idx[key]
		if !seen {
			idx[key] = len(out)
			out = append(out, entry{tag, w})
			continue
		}
		cur := out[i]
		if d, cd := math.Abs(w-1), math.Abs(cur.w-1); d > cd || (d == cd && w > cur.w) {
			out[i] = entry{tag, w}
		}
	}
	texts := make([]string, len(out))
	for i, e := range out {
		texts[i] = e.text
	}
	return strings.Join(texts, ", ")
}

// normalizeTag returns the canonical tag, its deduplication key (the text
// without weight, lowercased) and its weight (1 when it has none).
func normalizeTag(t string) (tag, key string, weight float64) {
	t = strings.Join(strings.Fields(t), " ")
	if t == "" {
		return "", "", 1
	}
	if m := weightRe.FindStringSubmatch(t); m != nil {
		if w, err := strconv.ParseFloat(m[2], 64); err == nil {
			inner := strings.TrimSpace(m[1])
			if w == 1 {
				return inner, strings.ToLower(inner), 1
			}
			return "(" + inner + ":" + strconv.FormatFloat(w, 'f', -1, 64) + ")", strings.ToLower(inner), w
		}
	}
	return t, strings.ToLower(t), 1
}

var closerOf = map[byte]byte{'(': ')', '[': ']', '<': '>'}

// matchedBrackets returns the positions of the brackets that have a partner.
// A lone "[" or "(" in a tag is plain text and must not swallow the commas
// that follow it.
func matchedBrackets(s string) map[int]bool {
	type open struct {
		i int
		c byte
	}
	var stack []open
	m := map[int]bool{}
	escaped := false
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch c := s[i]; c {
		case '\\':
			escaped = true
		case '(', '[', '<':
			stack = append(stack, open{i, c})
		case ')', ']', '>':
			if n := len(stack); n > 0 && closerOf[stack[n-1].c] == c {
				m[stack[n-1].i], m[i] = true, true
				stack = stack[:n-1]
			}
		}
	}
	return m
}

func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	escaped := false
	matched := matchedBrackets(s)
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch s[i] {
		case '\\':
			escaped = true
		case '(', '[', '<':
			if matched[i] {
				depth++
			}
		case ')', ']', '>':
			if matched[i] && depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
