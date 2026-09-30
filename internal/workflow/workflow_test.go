package workflow

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func loadExample(t *testing.T) Graph {
	t.Helper()
	raw, err := os.ReadFile("../../examples/workflow_api.example.json")
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestParseRejectsUIFormat(t *testing.T) {
	_, err := Parse([]byte(`{"nodes":[],"links":[],"version":0.4}`))
	if err != ErrUIFormat {
		t.Fatalf("got %v, want ErrUIFormat", err)
	}
}

func TestParseUnwrapsPromptEnvelope(t *testing.T) {
	g, err := Parse([]byte(`{"client_id":"x","prompt":{"1":{"class_type":"A","inputs":{}}}}`))
	if err != nil || len(g) != 1 {
		t.Fatalf("g=%v err=%v", g, err)
	}
}

func TestParseDetectsDanglingLink(t *testing.T) {
	_, err := Parse([]byte(`{"1":{"class_type":"A","inputs":{"x":["9",0]}}}`))
	if err == nil || !strings.Contains(err.Error(), "missing node") {
		t.Fatalf("got %v", err)
	}
}

func TestSuggestFindsPromptsAndSettings(t *testing.T) {
	g := loadExample(t)
	got := map[string]string{}
	for _, b := range Suggest(g) {
		got[b.Name] = b.NodeID + "." + b.Input
	}
	want := map[string]string{
		"seed": "3.seed", "steps": "3.steps", "cfg": "3.cfg",
		"sampler_name": "3.sampler_name", "scheduler": "3.scheduler", "denoise": "3.denoise",
		"positive": "6.text", "negative": "7.text",
		"width": "5.width", "height": "5.height", "batch_size": "5.batch_size",
		"checkpoint": "4.ckpt_name",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if err := g.ValidateBindings(Suggest(g)); err != nil {
		t.Fatal(err)
	}
}

func TestApplyInjectsWithoutMutatingSource(t *testing.T) {
	g := loadExample(t)
	bs := Suggest(g)
	out, res, err := Apply(g, bs, map[string]any{"positive": "1girl, solo", "steps": float64(30), "seed": float64(-1)})
	if err != nil {
		t.Fatal(err)
	}
	if out["6"].Inputs["text"] != "1girl, solo" {
		t.Fatalf("positive not injected: %v", out["6"].Inputs["text"])
	}
	if out["3"].Inputs["steps"] != int64(30) {
		t.Fatalf("steps: %#v", out["3"].Inputs["steps"])
	}
	seed, ok := res["seed"].(int64)
	if !ok || seed < 0 || out["3"].Inputs["seed"] != seed {
		t.Fatalf("seed not resolved consistently: %v vs %v", res["seed"], out["3"].Inputs["seed"])
	}
	if g["6"].Inputs["text"] != "placeholder positive prompt" {
		t.Fatal("source graph was mutated")
	}
	if out["3"].Inputs["cfg"] != g["3"].Inputs["cfg"] {
		t.Fatal("unbound parameter should keep its template value")
	}
}

func TestApplyErrors(t *testing.T) {
	g := loadExample(t)
	bs := Suggest(g)
	cases := map[string]map[string]any{
		"required":    {},
		"unknown":     {"positive": "x", "nope": 1},
		"wrong type":  {"positive": "x", "steps": "many"},
		"non integer": {"positive": "x", "steps": 20.5},
	}
	for name, params := range cases {
		if _, _, err := Apply(g, bs, params); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestValidateBindingsRejectsLinkedInput(t *testing.T) {
	g := loadExample(t)
	err := g.ValidateBindings([]Binding{{Name: "m", NodeID: "3", Input: "model", Type: TypeString}})
	if err == nil || !strings.Contains(err.Error(), "wired") {
		t.Fatalf("got %v", err)
	}
}

func TestSharedNameFeedsSeveralInputs(t *testing.T) {
	g := loadExample(t)
	bs := []Binding{
		{Name: "positive", NodeID: "6", Input: "text", Type: TypeString, Required: true},
		{Name: "positive", NodeID: "7", Input: "text", Type: TypeString},
	}
	out, _, err := Apply(g, bs, map[string]any{"positive": "same"})
	if err != nil {
		t.Fatal(err)
	}
	if out["6"].Inputs["text"] != "same" || out["7"].Inputs["text"] != "same" {
		t.Fatal("shared binding not applied to both nodes")
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"same"`) {
		t.Fatal("unexpected marshal output")
	}
}
