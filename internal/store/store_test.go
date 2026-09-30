package store

import (
	"errors"
	"os"
	"testing"

	"github.com/Gaetan1303/comfyvault/internal/workflow"
)

func example(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../examples/workflow_api.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPublishVersioning(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := example(t)

	_, v1, created, err := st.PublishWorkflow("wf", "First", "", raw, nil)
	if err != nil || !created || v1.Version != 1 {
		t.Fatalf("v1: %+v created=%v err=%v", v1, created, err)
	}
	_, same, created, err := st.PublishWorkflow("wf", "", "", raw, nil)
	if err != nil || created || same.Version != 1 {
		t.Fatalf("identical publish should be a no-op: %+v created=%v err=%v", same, created, err)
	}

	trimmed := workflow.Suggest(mustParse(t, raw))[:3]
	w, v2, created, err := st.PublishWorkflow("wf", "", "", raw, trimmed)
	if err != nil || !created || v2.Version != 2 {
		t.Fatalf("changed bindings should create v2: %+v created=%v err=%v", v2, created, err)
	}
	if w.Name != "First" {
		t.Fatalf("name should survive a publish without one, got %q", w.Name)
	}
	if got, ok := w.Get(1); !ok || got.Hash != v1.Hash {
		t.Fatal("version 1 must stay retrievable")
	}
}

func TestPublishRejects(t *testing.T) {
	st, _ := Open(t.TempDir())
	raw := example(t)
	if _, _, _, err := st.PublishWorkflow("Bad Slug", "", "", raw, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad slug: %v", err)
	}
	bad := []workflow.Binding{{Name: "x", NodeID: "99", Input: "text", Type: workflow.TypeString}}
	if _, _, _, err := st.PublishWorkflow("wf", "", "", raw, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("binding to a missing node: %v", err)
	}
	if _, err := st.GetWorkflow("../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("path traversal must not resolve: %v", err)
	}
}

func TestTableRoundTrip(t *testing.T) {
	st, _ := Open(t.TempDir())
	if _, err := st.Blocks.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := st.Blocks.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func mustParse(t *testing.T, raw []byte) workflow.Graph {
	t.Helper()
	g, err := workflow.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
