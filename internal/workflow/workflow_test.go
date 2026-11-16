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

func TestSeedDefaultIsHonoured(t *testing.T) {
	g := Graph{"1": {ClassType: "KSampler", Inputs: map[string]any{"seed": float64(0)}}}
	bs := []Binding{{Name: "seed", NodeID: "1", Input: "seed", Type: TypeSeed, Default: float64(1234)}}
	_, res, err := Apply(g, bs, nil)
	if err != nil || res["seed"] != int64(1234) {
		t.Fatalf("a seed default must be used when nothing is given: %v, %v", res, err)
	}
	_, res, _ = Apply(g, bs, map[string]any{"seed": float64(7)})
	if res["seed"] != int64(7) {
		t.Fatalf("an explicit seed wins over the default: %v", res)
	}
	_, res, _ = Apply(g, bs, map[string]any{"seed": float64(-1)})
	if res["seed"] == int64(1234) || res["seed"] == int64(-1) {
		t.Fatalf("-1 still means random: %v", res)
	}
}

func TestValidateRejectsTwoNamesOnOneInput(t *testing.T) {
	g := Graph{"3": {ClassType: "KSampler", Inputs: map[string]any{"steps": float64(20)}}}
	err := g.ValidateBindings([]Binding{
		{Name: "steps", NodeID: "3", Input: "steps", Type: TypeInt},
		{Name: "etapes", NodeID: "3", Input: "steps", Type: TypeInt},
	})
	if err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("got %v", err)
	}
}

func TestSuggestBindsTheSeedOfEverySampler(t *testing.T) {
	sampler := func(seed, steps float64) Node {
		return Node{ClassType: "KSampler", Inputs: map[string]any{
			"seed": seed, "steps": steps, "cfg": float64(7), "sampler_name": "euler", "scheduler": "normal", "denoise": float64(1)}}
	}
	g := Graph{"1": sampler(1, 20), "2": sampler(2, 10)}
	bs := Suggest(g)
	seeds, steps := 0, 0
	for _, b := range bs {
		switch b.Name {
		case "seed":
			seeds++
		case "steps":
			steps++
		}
	}
	if seeds != 2 || steps != 1 {
		t.Fatalf("want a seed per sampler and settings for the first only, got %d seeds, %d steps: %+v", seeds, steps, bs)
	}
	if err := g.ValidateBindings(bs); err != nil {
		t.Fatal(err)
	}
	out, _, err := Apply(g, bs, nil)
	if err != nil || out["1"].Inputs["seed"] != out["2"].Inputs["seed"] || out["1"].Inputs["seed"] == float64(1) {
		t.Fatalf("one seed value must drive both samplers: %v, %v", out, err)
	}
}
