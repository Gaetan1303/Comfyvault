package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/api"
	"github.com/Gaetan1303/comfyvault/internal/comfy"
	"github.com/Gaetan1303/comfyvault/internal/runner"
	"github.com/Gaetan1303/comfyvault/internal/store"
)

// fakeComfy answers like ComfyUI: /prompt returns an id, /history is empty on
// the first poll and complete afterwards.
type fakeComfy struct {
	mu      sync.Mutex
	prompts []map[string]struct {
		ClassType string         `json:"class_type"`
		Inputs    map[string]any `json:"inputs"`
	}
	polls    int
	rejectAs string

	hold       bool // history stays empty: the run never finishes
	pending    bool // /queue reports the prompt as waiting instead of running
	interrupts []string
	deleted    []string
}

func (f *fakeComfy) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		if f.rejectAs != "" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, f.rejectAs)
			return
		}
		var body struct {
			Prompt map[string]struct {
				ClassType string         `json:"class_type"`
				Inputs    map[string]any `json:"inputs"`
			} `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.prompts = append(f.prompts, body.Prompt)
		f.mu.Unlock()
		io.WriteString(w, `{"prompt_id":"p1","number":1,"node_errors":{}}`)
	})
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		n := f.polls
		hold := f.hold
		f.mu.Unlock()
		if hold || n < 2 {
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"p1":{"status":{"status_str":"success","completed":true,"messages":[]},
			"outputs":{"9":{"images":[{"filename":"out_00001_.png","subfolder":"","type":"output"}]},
			           "10":{"images":[{"filename":"tmp.png","subfolder":"","type":"temp"}]}}}}`)
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		pending := f.pending
		f.mu.Unlock()
		if pending {
			io.WriteString(w, `{"queue_running":[],"queue_pending":[[1,"p1",{}]]}`)
			return
		}
		io.WriteString(w, `{"queue_running":[[0,"p1",{}]],"queue_pending":[]}`)
	})
	mux.HandleFunc("POST /queue", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Delete []string `json:"delete"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.deleted = append(f.deleted, body.Delete...)
		f.mu.Unlock()
	})
	mux.HandleFunc("POST /interrupt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PromptID string `json:"prompt_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.interrupts = append(f.interrupts, body.PromptID)
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filename") != "out_00001_.png" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "PNGDATA")
	})
	mux.HandleFunc("GET /object_info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"KSampler":{},"CheckpointLoaderSimple":{},"EmptyLatentImage":{},"CLIPTextEncode":{},"VAEDecode":{}}`)
	})
	mux.HandleFunc("GET /system_stats", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	return mux
}

type harness struct {
	t    *testing.T
	fake *fakeComfy
	srv  *httptest.Server
}

func newHarness(t *testing.T, apiKey string) *harness {
	t.Helper()
	fake := &fakeComfy{}
	cs := httptest.NewServer(fake.handler())
	t.Cleanup(cs.Close)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(st, comfy.New(cs.URL), 5*time.Millisecond, 5*time.Second, log)
	t.Cleanup(rn.Close)
	srv := httptest.NewServer(api.New(st, comfy.New(cs.URL), rn, apiKey, log).Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, fake: fake, srv: srv}
}

func (h *harness) call(method, path string, body any, key string) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *harness) expect(want int, method, path string, body any) []byte {
	h.t.Helper()
	code, b := h.call(method, path, body, "")
	if code != want {
		h.t.Fatalf("%s %s: got %d, want %d\n%s", method, path, code, want, b)
	}
	return b
}

func exampleWorkflow(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../../examples/workflow_api.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t, "")

	h.expect(201, "POST", "/api/v1/blocks", map[string]any{"id": "quality", "text": "masterpiece, best quality"})
	h.expect(201, "POST", "/api/v1/blocks", map[string]any{"id": "subject", "text": "1girl, {{hair}} hair, masterpiece"})
	h.expect(201, "POST", "/api/v1/presets", map[string]any{"id": "portrait", "blocks": []string{"quality", "subject"}})
	h.expect(409, "POST", "/api/v1/blocks", map[string]any{"id": "quality", "text": "x"})
	h.expect(409, "DELETE", "/api/v1/blocks/quality", nil)
	h.expect(422, "POST", "/api/v1/presets", map[string]any{"id": "bad", "blocks": []string{"nope"}})

	b := h.expect(201, "POST", "/api/v1/workflows", map[string]any{"slug": "txt2img", "name": "Text to image", "api": exampleWorkflow(t)})
	if !strings.Contains(string(b), `"positive"`) {
		t.Fatalf("bindings were not suggested: %s", b)
	}
	h.expect(200, "POST", "/api/v1/workflows", map[string]any{"slug": "txt2img", "api": exampleWorkflow(t)}) // same content: no new version

	var chk struct {
		OK      bool     `json:"ok"`
		Missing []string `json:"missing_nodes"`
	}
	json.Unmarshal(h.expect(200, "GET", "/api/v1/workflows/txt2img/check", nil), &chk)
	if chk.OK || len(chk.Missing) != 1 || chk.Missing[0] != "SaveImage" {
		t.Fatalf("expected only SaveImage to be reported missing, got %+v", chk)
	}

	var rendered struct{ Text string }
	json.Unmarshal(h.expect(200, "POST", "/api/v1/prompts/render", map[string]any{"preset": "portrait", "vars": map[string]string{"hair": "silver"}}), &rendered)
	if want := "masterpiece, best quality, 1girl, silver hair"; rendered.Text != want {
		t.Fatalf("render: got %q, want %q", rendered.Text, want)
	}

	h.expect(422, "POST", "/api/v1/workflows/txt2img/run", map[string]any{"positive": map[string]any{"preset": "portrait"}}) // {{hair}} missing
	h.expect(404, "POST", "/api/v1/workflows/unknown/run", map[string]any{})

	var run store.Run
	json.Unmarshal(h.expect(202, "POST", "/api/v1/workflows/txt2img/run", map[string]any{
		"positive": map[string]any{"preset": "portrait", "vars": map[string]string{"hair": "silver"}},
		"params":   map[string]any{"steps": 12, "seed": 42},
	}), &run)
	if run.PromptID != "p1" || run.Params["seed"] != float64(42) {
		t.Fatalf("unexpected run: %+v", run)
	}

	deadline := time.Now().Add(5 * time.Second)
	for run.Status != store.StatusSucceeded {
		if run.Status == store.StatusFailed || time.Now().After(deadline) {
			t.Fatalf("run did not succeed: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
		json.Unmarshal(h.expect(200, "GET", "/api/v1/runs/"+run.ID, nil), &run)
	}
	if len(run.Outputs) != 1 || run.Outputs[0].Name != "out_00001_.png" {
		t.Fatalf("temp outputs should be skipped: %+v", run.Outputs)
	}
	if got := h.expect(200, "GET", "/api/v1/runs/"+run.ID+"/outputs/out_00001_.png", nil); string(got) != "PNGDATA" {
		t.Fatalf("output content: %q", got)
	}
	h.expect(404, "GET", "/api/v1/runs/"+run.ID+"/outputs/other.png", nil)

	h.fake.mu.Lock()
	sent := h.fake.prompts[0]
	h.fake.mu.Unlock()
	if got := sent["6"].Inputs["text"]; got != "masterpiece, best quality, 1girl, silver hair" {
		t.Fatalf("prompt sent to ComfyUI: %v", got)
	}
	if sent["3"].Inputs["steps"] != float64(12) || sent["3"].Inputs["seed"] != float64(42) {
		t.Fatalf("sampler inputs: %v", sent["3"].Inputs)
	}
}

func TestComfyValidationErrorIsForwarded(t *testing.T) {
	h := newHarness(t, "")
	h.fake.rejectAs = `{"error":{"type":"prompt_outputs_failed_validation"},"node_errors":{"4":{}}}`
	h.expect(201, "POST", "/api/v1/workflows", map[string]any{"slug": "w", "api": exampleWorkflow(t)})
	b := h.expect(422, "POST", "/api/v1/workflows/w/run", map[string]any{"params": map[string]any{"positive": "x"}})
	if !strings.Contains(string(b), "prompt_outputs_failed_validation") {
		t.Fatalf("ComfyUI detail lost: %s", b)
	}
}

func TestRejectsUIFormatAndUnknownFields(t *testing.T) {
	h := newHarness(t, "")
	b := h.expect(422, "POST", "/api/v1/workflows", map[string]any{"slug": "ui", "api": json.RawMessage(`{"nodes":[],"links":[]}`)})
	if !strings.Contains(string(b), "API Format") {
		t.Fatalf("message should explain the export: %s", b)
	}
	h.expect(400, "POST", "/api/v1/blocks", map[string]any{"id": "x", "text": "y", "txt": "typo"})
}

func TestAPIKey(t *testing.T) {
	h := newHarness(t, "s3cret")
	if code, _ := h.call("GET", "/api/v1/workflows", nil, ""); code != 401 {
		t.Fatalf("no key: got %d", code)
	}
	if code, _ := h.call("GET", "/api/v1/workflows", nil, "wrong"); code != 401 {
		t.Fatalf("wrong key: got %d", code)
	}
	if code, _ := h.call("GET", "/api/v1/workflows", nil, "s3cret"); code != 200 {
		t.Fatalf("right key: got %d", code)
	}
	if code, _ := h.call("GET", "/healthz", nil, ""); code != 200 {
		t.Fatalf("healthz should stay open: got %d", code)
	}
}

// waitDone polls a run until it reaches a final status.
func (h *harness) waitDone(id string) store.Run {
	h.t.Helper()
	var run store.Run
	deadline := time.Now().Add(5 * time.Second)
	for {
		json.Unmarshal(h.expect(200, "GET", "/api/v1/runs/"+id, nil), &run)
		if run.Finished() {
			return run
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run %s did not finish: %+v", id, run)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) startRun(params map[string]any) store.Run {
	h.t.Helper()
	var run store.Run
	json.Unmarshal(h.expect(202, "POST", "/api/v1/workflows/w/run", map[string]any{"params": params}), &run)
	return run
}

func TestCancelRun(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "running"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.fake.mu.Lock()
			h.fake.hold, h.fake.pending = true, pending
			h.fake.mu.Unlock()
			h.expect(201, "POST", "/api/v1/workflows", map[string]any{"slug": "w", "api": exampleWorkflow(t)})

			run := h.startRun(map[string]any{"positive": "x"})
			h.expect(409, "PATCH", "/api/v1/runs/"+run.ID, map[string]any{"favorite": true}) // not finished yet

			var got store.Run
			json.Unmarshal(h.expect(200, "POST", "/api/v1/runs/"+run.ID+"/cancel", nil), &got)
			if got.Status != store.StatusCancelled || got.FinishedAt == nil {
				t.Fatalf("expected a cancelled run, got %+v", got)
			}

			h.fake.mu.Lock()
			interrupts, deleted := len(h.fake.interrupts), len(h.fake.deleted)
			h.fake.mu.Unlock()
			if pending && (deleted != 1 || interrupts != 0) {
				t.Fatalf("a queued prompt should be removed from the queue: deleted=%d interrupts=%d", deleted, interrupts)
			}
			if !pending && (interrupts != 1 || deleted != 0) {
				t.Fatalf("a running prompt should be interrupted: deleted=%d interrupts=%d", deleted, interrupts)
			}

			h.expect(409, "POST", "/api/v1/runs/"+run.ID+"/cancel", nil) // already cancelled
			h.expect(404, "POST", "/api/v1/runs/20000101-000000-abcdef/cancel", nil)
		})
	}
}

func TestReplayRun(t *testing.T) {
	h := newHarness(t, "")
	h.expect(201, "POST", "/api/v1/workflows", map[string]any{"slug": "w", "api": exampleWorkflow(t)})

	first := h.startRun(map[string]any{"positive": "x", "steps": 12, "seed": 42})
	h.waitDone(first.ID)

	var same store.Run
	json.Unmarshal(h.expect(202, "POST", "/api/v1/runs/"+first.ID+"/replay", nil), &same)
	if same.ReplayOf != first.ID || same.Version != first.Version ||
		same.Params["seed"] != float64(42) || same.Params["steps"] != float64(12) || same.Params["positive"] != "x" {
		t.Fatalf("replay should reuse the exact parameters: %+v", same)
	}

	var varied store.Run
	json.Unmarshal(h.expect(202, "POST", "/api/v1/runs/"+first.ID+"/replay",
		map[string]any{"new_seed": true, "params": map[string]any{"steps": 20}}), &varied)
	if varied.Params["seed"] == float64(42) || varied.Params["steps"] != float64(20) {
		t.Fatalf("new_seed and overrides were not applied: %+v", varied.Params)
	}

	h.expect(400, "POST", "/api/v1/runs/"+first.ID+"/replay", map[string]any{"typo": true})
	h.expect(422, "POST", "/api/v1/runs/"+first.ID+"/replay", map[string]any{"params": map[string]any{"nope": 1}})
	h.expect(404, "POST", "/api/v1/runs/20000101-000000-abcdef/replay", nil)
}

func TestBatch(t *testing.T) {
	h := newHarness(t, "")
	h.expect(201, "POST", "/api/v1/workflows", map[string]any{"slug": "w", "api": exampleWorkflow(t)})

	var out struct {
		BatchID string      `json:"batch_id"`
		Count   int         `json:"count"`
		Runs    []store.Run `json:"runs"`
	}
	json.Unmarshal(h.expect(202, "POST", "/api/v1/workflows/w/batch", map[string]any{
		"params": map[string]any{"positive": "x"},
		"grid":   map[string]any{"steps": []int{10, 20}},
		"count":  2,
	}), &out)
	if out.Count != 4 || len(out.Runs) != 4 || out.BatchID == "" {
		t.Fatalf("expected 4 runs in one batch: %+v", out)
	}
	steps := map[float64]int{}
	for _, r := range out.Runs {
		if r.BatchID != out.BatchID {
			t.Fatalf("run %s lost its batch id", r.ID)
		}
		steps[r.Params["steps"].(float64)]++
	}
	if steps[10] != 2 || steps[20] != 2 {
		t.Fatalf("grid values were not spread over the runs: %v", steps)
	}
	if out.Runs[0].Params["seed"] == out.Runs[1].Params["seed"] {
		t.Fatalf("repeated runs should get different random seeds")
	}

	for _, r := range out.Runs {
		h.waitDone(r.ID)
	}
	var listed []store.Run
	json.Unmarshal(h.expect(200, "GET", "/api/v1/runs?batch="+out.BatchID, nil), &listed)
	if len(listed) != 4 {
		t.Fatalf("batch filter: got %d runs", len(listed))
	}

	tooMany := make([]int, 100)
	h.expect(422, "POST", "/api/v1/workflows/w/batch", map[string]any{"params": map[string]any{"positive": "x"}, "grid": map[string]any{"steps": tooMany}})
	h.expect(422, "POST", "/api/v1/workflows/w/batch", map[string]any{"params": map[string]any{"positive": "x"}, "grid": map[string]any{"steps": []int{}}})
	h.expect(422, "POST", "/api/v1/workflows/w/batch", map[string]any{"params": map[string]any{"steps": 5}, "grid": map[string]any{"steps": []int{10}}})
	h.expect(422, "POST", "/api/v1/workflows/w/batch", map[string]any{"params": map[string]any{"positive": "x"}, "grid": map[string]any{"unknown": []int{1}}})
	h.expect(404, "POST", "/api/v1/workflows/nope/batch", map[string]any{})
}

func TestRunAnnotationsAndFilters(t *testing.T) {
	h := newHarness(t, "")
	h.expect(201, "POST", "/api/v1/workflows", map[string]any{"slug": "w", "api": exampleWorkflow(t)})
	run := h.startRun(map[string]any{"positive": "x"})
	h.waitDone(run.ID)

	var got store.Run
	json.Unmarshal(h.expect(200, "PATCH", "/api/v1/runs/"+run.ID,
		map[string]any{"tags": []string{"Portrait", "keep"}, "favorite": true, "note": "nice hands"}), &got)
	if !got.Favorite || got.Note != "nice hands" || len(got.Tags) != 2 || got.Tags[0] != "portrait" {
		t.Fatalf("annotations were not saved: %+v", got)
	}
	h.expect(422, "PATCH", "/api/v1/runs/"+run.ID, map[string]any{"tags": []string{"not valid"}})
	h.expect(400, "PATCH", "/api/v1/runs/"+run.ID, map[string]any{"colour": "red"})

	count := func(query string) int {
		var runs []store.Run
		json.Unmarshal(h.expect(200, "GET", "/api/v1/runs"+query, nil), &runs)
		return len(runs)
	}
	if n := count("?tag=Portrait"); n != 1 {
		t.Fatalf("tag filter (case-insensitive): %d", n)
	}
	if n := count("?favorite=true&status=succeeded&workflow=w"); n != 1 {
		t.Fatalf("combined filters: %d", n)
	}
	if n := count("?tag=other"); n != 0 {
		t.Fatalf("unknown tag should match nothing: %d", n)
	}
	if n := count("?since=2999-01-01T00:00:00Z"); n != 0 {
		t.Fatalf("since in the future should match nothing: %d", n)
	}
	h.expect(400, "GET", "/api/v1/runs?status=bogus", nil)
	h.expect(400, "GET", "/api/v1/runs?since=yesterday", nil)
	h.expect(400, "GET", "/api/v1/runs?favorite=maybe", nil)
}
