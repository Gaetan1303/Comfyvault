// Package prompt composes positive/negative prompts from reusable blocks.
package prompt

import (
	"fmt"
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
// (tag:1.1). Commas inside (), [] and <> do not split.
func Normalize(s string) string {
	seen := map[string]bool{}
	var out []string
	for _, part := range splitTop(s) {
		tag, key := normalizeTag(part)
		if tag == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tag)
	}
	return strings.Join(out, ", ")
}

func normalizeTag(t string) (tag, key string) {
	t = strings.Join(strings.Fields(t), " ")
	if t == "" {
		return "", ""
	}
	if m := weightRe.FindStringSubmatch(t); m != nil {
		if w, err := strconv.ParseFloat(m[2], 64); err == nil {
			inner := strings.TrimSpace(m[1])
			if w == 1 {
				return inner, strings.ToLower(inner)
			}
			return "(" + inner + ":" + strconv.FormatFloat(w, 'f', -1, 64) + ")", strings.ToLower(inner)
		}
	}
	return t, strings.ToLower(t)
}

func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	escaped := false
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch s[i] {
		case '\\':
			escaped = true
		case '(', '[', '<':
			depth++
		case ')', ']', '>':
			if depth > 0 {
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
