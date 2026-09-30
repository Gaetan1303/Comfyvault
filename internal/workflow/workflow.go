// Package workflow handles ComfyUI workflows in API format: parsing,
// parameter bindings and value injection.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Node struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// Graph is a workflow in ComfyUI API format: node id -> node.
type Graph map[string]Node

// Binding exposes one node input as a named parameter.
type Binding struct {
	Name     string `json:"name"`
	NodeID   string `json:"node_id"`
	Input    string `json:"input"`
	Type     string `json:"type"`
	Default  any    `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
}

const (
	TypeString = "string"
	TypeInt    = "int"
	TypeFloat  = "float"
	TypeBool   = "bool"
	// TypeSeed is an integer that is drawn at random when omitted or -1.
	TypeSeed = "seed"
)

var ErrUIFormat = errors.New("this is a UI-format workflow; export it with \"Save (API Format)\" (enable dev mode options in the ComfyUI settings)")

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// Parse decodes an API-format workflow. It also accepts the {"prompt": {...}}
// envelope that ComfyUI itself sends to /prompt.
func Parse(raw []byte) (Graph, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, ok := top["nodes"]; ok {
		if _, ok := top["links"]; ok {
			return nil, ErrUIFormat
		}
	}
	if inner, ok := top["prompt"]; ok {
		raw = inner
	}
	var g Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("not an API-format workflow: %w", err)
	}
	if len(g) == 0 {
		return nil, errors.New("workflow has no nodes")
	}
	for id, n := range g {
		if n.ClassType == "" {
			return nil, fmt.Errorf("node %q has no class_type", id)
		}
		if n.Inputs == nil {
			n.Inputs = map[string]any{}
			g[id] = n
		}
	}
	for _, id := range sortedIDs(g) {
		for name, v := range g[id].Inputs {
			if src, _, ok := linkRef(v); ok {
				if _, exists := g[src]; !exists {
					return nil, fmt.Errorf("node %q input %q links to missing node %q", id, name, src)
				}
			}
		}
	}
	return g, nil
}

// ClassTypes returns the distinct node class names used by the graph.
func (g Graph) ClassTypes() []string {
	set := map[string]struct{}{}
	for _, n := range g {
		set[n.ClassType] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ValidateBindings checks that every binding points at a literal input of an
// existing node. Several bindings may share a name (one value feeding several
// inputs) as long as their types agree.
func (g Graph) ValidateBindings(bs []Binding) error {
	types := map[string]string{}
	for i, b := range bs {
		if !nameRe.MatchString(b.Name) {
			return fmt.Errorf("binding %d: name %q must match %s", i, b.Name, nameRe)
		}
		switch b.Type {
		case TypeString, TypeInt, TypeFloat, TypeBool, TypeSeed:
		default:
			return fmt.Errorf("binding %q: type %q must be one of string, int, float, bool, seed", b.Name, b.Type)
		}
		if prev, ok := types[b.Name]; ok && prev != b.Type {
			return fmt.Errorf("binding %q is declared with both %s and %s", b.Name, prev, b.Type)
		}
		types[b.Name] = b.Type

		n, ok := g[b.NodeID]
		if !ok {
			return fmt.Errorf("binding %q: node %q does not exist", b.Name, b.NodeID)
		}
		cur, ok := n.Inputs[b.Input]
		if !ok {
			return fmt.Errorf("binding %q: node %q (%s) has no input %q", b.Name, b.NodeID, n.ClassType, b.Input)
		}
		if _, _, link := linkRef(cur); link {
			return fmt.Errorf("binding %q: input %q of node %q is wired to another node; bind a literal input", b.Name, b.Input, b.NodeID)
		}
		if b.Default != nil {
			if _, err := coerce(b.Type, b.Default); err != nil {
				return fmt.Errorf("binding %q default: %w", b.Name, err)
			}
		}
	}
	return nil
}

// Apply returns a copy of g with params injected through the bindings, and
// the values that were actually used (seeds included), so a run can be
// reproduced later. g itself is not modified.
func Apply(g Graph, bs []Binding, params map[string]any) (Graph, map[string]any, error) {
	first := map[string]Binding{}
	for _, b := range bs {
		if _, ok := first[b.Name]; !ok {
			first[b.Name] = b
		}
	}
	for k := range params {
		if _, ok := first[k]; !ok {
			return nil, nil, fmt.Errorf("unknown parameter %q (available: %s)", k, strings.Join(paramNames(first), ", "))
		}
	}

	resolved := map[string]any{}
	for name, b := range first {
		v := params[name]
		if b.Type == TypeSeed {
			if v == nil || isMinusOne(v) {
				v = rand.Int64N(1 << 53)
			}
		} else if v == nil {
			switch {
			case b.Default != nil:
				v = b.Default
			case b.Required:
				return nil, nil, fmt.Errorf("parameter %q is required", name)
			default:
				continue
			}
		}
		cv, err := coerce(b.Type, v)
		if err != nil {
			return nil, nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		resolved[name] = cv
	}

	out, err := clone(g)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range bs {
		if v, ok := resolved[b.Name]; ok {
			out[b.NodeID].Inputs[b.Input] = v
		}
	}
	return out, resolved, nil
}

// Suggest proposes bindings for the usual suspects: the prompts feeding the
// first sampler, its main settings, the latent size, the checkpoint and LoRAs.
func Suggest(g Graph) []Binding {
	var out []Binding
	add := func(name, node, input, typ string, required bool) {
		n, ok := g[node]
		if !ok {
			return
		}
		cur, ok := n.Inputs[input]
		if !ok {
			return
		}
		if _, _, link := linkRef(cur); link {
			return
		}
		out = append(out, Binding{Name: name, NodeID: node, Input: input, Type: typ, Required: required})
	}

	ids := sortedIDs(g)
	for _, id := range ids {
		n := g[id]
		advanced := n.ClassType == "KSamplerAdvanced"
		if n.ClassType != "KSampler" && !advanced {
			continue
		}
		seedInput := "seed"
		if advanced {
			seedInput = "noise_seed"
		}
		add("seed", id, seedInput, TypeSeed, false)
		add("steps", id, "steps", TypeInt, false)
		add("cfg", id, "cfg", TypeFloat, false)
		add("sampler_name", id, "sampler_name", TypeString, false)
		add("scheduler", id, "scheduler", TypeString, false)
		if !advanced {
			add("denoise", id, "denoise", TypeFloat, false)
		}
		if src, _, ok := linkRef(n.Inputs["positive"]); ok {
			if t, ok := textEncoder(g, src, 0); ok {
				add("positive", t, "text", TypeString, true)
			}
		}
		if src, _, ok := linkRef(n.Inputs["negative"]); ok {
			if t, ok := textEncoder(g, src, 0); ok {
				add("negative", t, "text", TypeString, false)
			}
		}
		if src, _, ok := linkRef(n.Inputs["latent_image"]); ok {
			if l, ok := g[src]; ok && (l.ClassType == "EmptyLatentImage" || l.ClassType == "EmptySD3LatentImage") {
				add("width", src, "width", TypeInt, false)
				add("height", src, "height", TypeInt, false)
				add("batch_size", src, "batch_size", TypeInt, false)
			}
		}
		break
	}

	haveCkpt := false
	loras := 0
	for _, id := range ids {
		switch g[id].ClassType {
		case "CheckpointLoaderSimple":
			if !haveCkpt {
				add("checkpoint", id, "ckpt_name", TypeString, false)
				haveCkpt = true
			}
		case "LoraLoader":
			loras++
			add(fmt.Sprintf("lora_%d", loras), id, "lora_name", TypeString, false)
			add(fmt.Sprintf("lora_%d_strength", loras), id, "strength_model", TypeFloat, false)
		}
	}
	return out
}

// textEncoder follows conditioning links back to the CLIPTextEncode that
// produced them (through ControlNet apply nodes and the like).
func textEncoder(g Graph, id string, depth int) (string, bool) {
	n, ok := g[id]
	if !ok || depth > 8 {
		return "", false
	}
	if n.ClassType == "CLIPTextEncode" {
		return id, true
	}
	if src, _, ok := linkRef(n.Inputs["conditioning"]); ok {
		return textEncoder(g, src, depth+1)
	}
	return "", false
}

// linkRef recognises ComfyUI's ["node_id", output_index] connection form.
func linkRef(v any) (string, int, bool) {
	a, ok := v.([]any)
	if !ok || len(a) != 2 {
		return "", 0, false
	}
	id, ok := a[0].(string)
	if !ok {
		return "", 0, false
	}
	idx, ok := a[1].(float64)
	if !ok {
		return "", 0, false
	}
	return id, int(idx), true
}

func coerce(typ string, v any) (any, error) {
	switch typ {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string, got %T", v)
		}
		return s, nil
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected a boolean, got %T", v)
		}
		return b, nil
	case TypeInt, TypeSeed:
		switch x := v.(type) {
		case int:
			return int64(x), nil
		case int64:
			return x, nil
		case float64:
			if x != math.Trunc(x) || math.IsInf(x, 0) {
				return nil, fmt.Errorf("expected an integer, got %v", x)
			}
			return int64(x), nil
		}
		return nil, fmt.Errorf("expected an integer, got %T", v)
	case TypeFloat:
		switch x := v.(type) {
		case int:
			return float64(x), nil
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		}
		return nil, fmt.Errorf("expected a number, got %T", v)
	}
	return nil, fmt.Errorf("unsupported type %q", typ)
}

func isMinusOne(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == -1
	case int:
		return x == -1
	case int64:
		return x == -1
	}
	return false
}

func clone(g Graph) (Graph, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	var out Graph
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func paramNames(m map[string]Binding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedIDs orders node ids numerically when they are numbers, which is what
// ComfyUI produces, and lexically otherwise.
func sortedIDs(g Graph) []string {
	ids := make([]string, 0, len(g))
	for id := range g {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, ea := strconv.Atoi(ids[i])
		b, eb := strconv.Atoi(ids[j])
		switch {
		case ea == nil && eb == nil:
			return a < b
		case ea == nil:
			return true
		case eb == nil:
			return false
		}
		return ids[i] < ids[j]
	})
	return ids
}
