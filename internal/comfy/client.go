// Package comfy is a small client for the parts of the ComfyUI HTTP API that
// comfyvault needs.
package comfy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	base     string
	hc       *http.Client
	ClientID string
}

func New(baseURL string) *Client {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{},
		ClientID: "comfyvault-" + hex.EncodeToString(b[:]),
	}
}

// APIError is a non-2xx answer from ComfyUI. Body is kept because on a 400
// from /prompt it carries the per-node validation errors.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("comfyui returned %d: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks that ComfyUI answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/system_stats", nil, nil, 3*time.Second)
}

// Queue submits an API-format workflow and returns its prompt id.
func (c *Client) Queue(ctx context.Context, graph any) (string, error) {
	var resp struct {
		PromptID string `json:"prompt_id"`
	}
	body := map[string]any{"prompt": graph, "client_id": c.ClientID}
	if err := c.do(ctx, http.MethodPost, "/prompt", body, &resp, 30*time.Second); err != nil {
		return "", err
	}
	if resp.PromptID == "" {
		return "", fmt.Errorf("comfyui accepted the prompt but returned no prompt_id")
	}
	return resp.PromptID, nil
}

type FileRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type OutputFile struct {
	Node string
	FileRef
}

type HistoryEntry struct {
	Status struct {
		StatusStr string            `json:"status_str"`
		Completed bool              `json:"completed"`
		Messages  []json.RawMessage `json:"messages"`
	} `json:"status"`
	Outputs map[string]map[string]json.RawMessage `json:"outputs"`
}

// History returns the entry for promptID, or nil while ComfyUI has not
// recorded one yet (the prompt is still queued or executing).
func (c *Client) History(ctx context.Context, promptID string) (*HistoryEntry, error) {
	var m map[string]HistoryEntry
	if err := c.do(ctx, http.MethodGet, "/history/"+url.PathEscape(promptID), nil, &m, 30*time.Second); err != nil {
		return nil, err
	}
	e, ok := m[promptID]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

// Files lists every file produced by the run, in node id order. Output keys
// that are not file lists (text, animated flags...) are skipped.
func (h *HistoryEntry) Files() []OutputFile {
	nodes := make([]string, 0, len(h.Outputs))
	for n := range h.Outputs {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	var out []OutputFile
	for _, n := range nodes {
		keys := make([]string, 0, len(h.Outputs[n]))
		for k := range h.Outputs[n] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var refs []FileRef
			if json.Unmarshal(h.Outputs[n][k], &refs) != nil {
				continue
			}
			for _, r := range refs {
				if r.Filename != "" {
					out = append(out, OutputFile{Node: n, FileRef: r})
				}
			}
		}
	}
	return out
}

// ErrorMessage extracts the execution error from a failed entry.
func (h *HistoryEntry) ErrorMessage() string {
	for _, raw := range h.Status.Messages {
		var pair [2]json.RawMessage
		if json.Unmarshal(raw, &pair) != nil {
			continue
		}
		var kind string
		if json.Unmarshal(pair[0], &kind) != nil || kind != "execution_error" {
			continue
		}
		var d struct {
			Message  string `json:"exception_message"`
			NodeType string `json:"node_type"`
			NodeID   string `json:"node_id"`
		}
		if json.Unmarshal(pair[1], &d) == nil && d.Message != "" {
			return fmt.Sprintf("%s (node %s, %s)", strings.TrimSpace(d.Message), d.NodeID, d.NodeType)
		}
	}
	return "execution failed"
}

// Position reports whether promptID is "running", "pending" or neither ("").
func (c *Client) Position(ctx context.Context, promptID string) (string, error) {
	var q struct {
		Running [][]json.RawMessage `json:"queue_running"`
		Pending [][]json.RawMessage `json:"queue_pending"`
	}
	if err := c.do(ctx, http.MethodGet, "/queue", nil, &q, 30*time.Second); err != nil {
		return "", err
	}
	has := func(items [][]json.RawMessage) bool {
		for _, it := range items {
			var id string
			if len(it) > 1 && json.Unmarshal(it[1], &id) == nil && id == promptID {
				return true
			}
		}
		return false
	}
	switch {
	case has(q.Running):
		return "running", nil
	case has(q.Pending):
		return "pending", nil
	}
	return "", nil
}

// NodeClasses returns the set of node class names installed in ComfyUI.
func (c *Client) NodeClasses(ctx context.Context) (map[string]struct{}, error) {
	var m map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/object_info", nil, &m, 90*time.Second); err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out, nil
}

// View streams a generated file. The caller closes the reader.
func (c *Client) View(ctx context.Context, ref FileRef) (io.ReadCloser, error) {
	q := url.Values{"filename": {ref.Filename}, "subfolder": {ref.Subfolder}, "type": {ref.Type}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/view?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return resp.Body, nil
}
