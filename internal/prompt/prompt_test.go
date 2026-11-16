package prompt

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"masterpiece,  best quality ,, masterpiece", "masterpiece, best quality"},
		{"Solo, solo, SOLO", "Solo"},
		{"(smile:1.0), (blush:1.10), blush", "smile, (blush:1.1)"},
		{"(a, b:1.2), c", "(a, b:1.2), c"},
		{"<lora:foo:0.8>, 1girl", "<lora:foo:0.8>, 1girl"},
		{`rating \(x\), y`, `rating \(x\), y`},
		{"line one\n  line two, tag", "line one line two, tag"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderReportsAllMissing(t *testing.T) {
	_, err := Render("{{ a }}, {{b}}, {{a}}", map[string]string{})
	if err == nil || err.Error() != "missing variables: a, b" {
		t.Fatalf("got %v", err)
	}
	got, err := Render("{{a}} and {{ b }}", map[string]string{"a": "1", "b": "2"})
	if err != nil || got != "1 and 2" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestCompose(t *testing.T) {
	blocks := []Block{
		{ID: "q", Text: "masterpiece, best quality"},
		{ID: "c", Text: "1girl, {{hair}} hair, masterpiece"},
	}
	got, err := Compose(blocks, map[string]string{"hair": "silver"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "masterpiece, best quality, 1girl, silver hair"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := Compose(blocks, nil); err == nil {
		t.Fatal("expected missing variable error")
	}
}

func TestNormalizeKeepsEmphasisWhateverTheOrder(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cat, (cat:1.4)", "(cat:1.4)"},
		{"(cat:1.4), cat", "(cat:1.4)"},
		{"(cat:1.2), (cat:1.4)", "(cat:1.4)"},
		{"cat, (cat:0.5)", "(cat:0.5)"},
		{"a, (cat:1.4), b, cat", "a, (cat:1.4), b"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeToleratesUnbalancedBrackets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"girl [looking away, smile, smile", "girl [looking away, smile"},
		{"1girl, (smile, solo, solo", "1girl, (smile, solo"},
		{"a > b, c, c", "a > b, c"},
		{"(a, b:1.2), c, c", "(a, b:1.2), c"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
