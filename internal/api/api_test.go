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
		f.mu.Unlock()
		if n < 2 {
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"p1":{"status":{"status_str":"success","completed":true,"messages":[]},
			"outputs":{"9":{"images":[{"filename":"out_00001_.png","subfolder":"","type":"output"}]},
			           "10":{"images":[{"filename":"tmp.png","subfolder":"","type":"temp"}]}}}}`)
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"queue_running":[[0,"p1",{}]],"queue_pending":[]}`)
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
