// Package api exposes comfyvault over HTTP/JSON.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/comfy"
	"github.com/Gaetan1303/comfyvault/internal/prompt"
	"github.com/Gaetan1303/comfyvault/internal/runner"
	"github.com/Gaetan1303/comfyvault/internal/store"
	"github.com/Gaetan1303/comfyvault/internal/workflow"
)

const maxBody = 8 << 20

type Server struct {
	st     *store.Store
	cl     *comfy.Client
	rn     *runner.Runner
	apiKey string
	log    *slog.Logger
}

func New(st *store.Store, cl *comfy.Client, rn *runner.Runner, apiKey string, log *slog.Logger) *Server {
	return &Server{st: st, cl: cl, rn: rn, apiKey: apiKey, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)

	mux.HandleFunc("GET /api/v1/workflows", s.listWorkflows)
	mux.HandleFunc("POST /api/v1/workflows", s.createWorkflow)
	mux.HandleFunc("GET /api/v1/workflows/{slug}", s.getWorkflow)
	mux.HandleFunc("GET /api/v1/workflows/{slug}/check", s.checkWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/{slug}/run", s.runWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/{slug}/batch", s.batchWorkflow)

	mux.HandleFunc("POST /api/v1/prompts/render", s.renderPrompt)

	mountCRUD(s, mux, "/api/v1/blocks", entity[prompt.Block]{
		table: s.st.Blocks,
		id:    func(b prompt.Block) string { return b.ID },
		setID: func(b *prompt.Block, id string) { b.ID = id },
		check: func(b prompt.Block) error {
			if strings.TrimSpace(b.Text) == "" {
				return errors.New("text is required")
			}
			return nil
		},
		beforeDelete: s.blockNotInUse,
	})
	mountCRUD(s, mux, "/api/v1/presets", entity[prompt.Preset]{
		table: s.st.Presets,
		id:    func(p prompt.Preset) string { return p.ID },
		setID: func(p *prompt.Preset, id string) { p.ID = id },
		check: func(p prompt.Preset) error {
			if len(p.Blocks) == 0 {
				return errors.New("a preset needs at least one block")
			}
			for _, id := range p.Blocks {
				if _, err := s.st.Blocks.Get(id); err != nil {
					return fmt.Errorf("block %q: %w", id, err)
				}
			}
			return nil
		},
	})

	mux.HandleFunc("GET /api/v1/runs", s.listRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	mux.HandleFunc("PATCH /api/v1/runs/{id}", s.patchRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/replay", s.replayRun)
	mux.HandleFunc("GET /api/v1/runs/{id}/outputs/{name}", s.getOutput)

	return s.auth(mux)
}

// Middleware

func (s *Server) auth(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	want := []byte("Bearer " + s.apiKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="comfyvault"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decode reads a strict JSON body; unknown fields are rejected so typos in a
// request do not silently do nothing.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// decodeOptional is decode for endpoints whose body may be left out.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ae *comfy.APIError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, workflow.ErrUIFormat):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.As(err, &ae):
		body := map[string]any{"error": "ComfyUI rejected the workflow", "comfyui_status": ae.Status}
		var detail json.RawMessage
		if json.Unmarshal([]byte(ae.Body), &detail) == nil {
			body["comfyui"] = detail
		} else {
			body["comfyui"] = ae.Body
		}
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case errors.Is(err, runner.ErrUpstream):
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(format, args...))
}

// Health

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	comfyState := map[string]any{"reachable": true}
	if err := s.cl.Ping(ctx); err != nil {
		comfyState = map[string]any{"reachable": false, "error": err.Error()}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "comfyui": comfyState})
}

// Workflows

type workflowSummary struct {
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	LatestVersion int      `json:"latest_version"`
	Parameters    []string `json:"parameters"`
}

func paramNames(bs []workflow.Binding) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, b := range bs {
		if !seen[b.Name] {
			seen[b.Name] = true
			out = append(out, b.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	all, err := s.st.ListWorkflows()
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]workflowSummary, 0, len(all))
	for _, wf := range all {
		l := wf.Latest()
		out = append(out, workflowSummary{wf.Slug, wf.Name, wf.Description, l.Version, paramNames(l.Bindings)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug        string             `json:"slug"`
		Name        string             `json:"name"`
		Description string             `json:"description"`
		API         json.RawMessage    `json:"api"`
		Bindings    []workflow.Binding `json:"bindings"`
	}
	if !decode(w, r, &req) {
		return
	}
	wf, v, created, err := s.st.PublishWorkflow(req.Slug, req.Name, req.Description, req.API, req.Bindings)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"slug": wf.Slug, "version": v.Version, "created": created, "hash": v.Hash, "bindings": v.Bindings,
	})
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := s.st.GetWorkflow(r.PathValue("slug"))
	if err != nil {
		s.fail(w, err)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("version"))
	v, ok := wf.Get(n)
	if !ok {
		s.fail(w, fmt.Errorf("%w: workflow %q has no version %d", store.ErrNotFound, wf.Slug, n))
		return
	}
	type ver struct {
		Version   int       `json:"version"`
		Hash      string    `json:"hash"`
		CreatedAt time.Time `json:"created_at"`
	}
	versions := make([]ver, 0, len(wf.Versions))
	for _, x := range wf.Versions {
		versions = append(versions, ver{x.Version, x.Hash, x.CreatedAt})
	}
	body := map[string]any{
		"slug": wf.Slug, "name": wf.Name, "description": wf.Description,
		"versions": versions, "version": v.Version, "bindings": v.Bindings,
	}
	if r.URL.Query().Get("api") == "1" {
		body["api"] = v.API
	}
	writeJSON(w, http.StatusOK, body)
}

// checkWorkflow compares the node classes a workflow uses with what the
// connected ComfyUI has installed.
func (s *Server) checkWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := s.st.GetWorkflow(r.PathValue("slug"))
	if err != nil {
		s.fail(w, err)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("version"))
	v, ok := wf.Get(n)
	if !ok {
		s.fail(w, fmt.Errorf("%w: workflow %q has no version %d", store.ErrNotFound, wf.Slug, n))
		return
	}
	g, err := workflow.Parse(v.API)
	if err != nil {
		s.fail(w, err)
		return
	}
	installed, err := s.cl.NodeClasses(r.Context())
	if err != nil {
		s.fail(w, fmt.Errorf("%w: %v", runner.ErrUpstream, err))
		return
	}
	missing := []string{}
	for _, c := range g.ClassTypes() {
		if _, ok := installed[c]; !ok {
			missing = append(missing, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v.Version, "ok": len(missing) == 0, "missing_nodes": missing})
}

type promptRef struct {
	Preset string            `json:"preset,omitempty"`
	Blocks []string          `json:"blocks,omitempty"`
	Vars   map[string]string `json:"vars,omitempty"`
}

func (s *Server) resolvePrompt(ref promptRef) (string, error) {
	ids := ref.Blocks
	switch {
	case ref.Preset != "" && len(ref.Blocks) > 0:
		return "", invalid("give either a preset or a list of blocks, not both")
	case ref.Preset != "":
		p, err := s.st.Presets.Get(ref.Preset)
		if err != nil {
			return "", err
		}
		ids = p.Blocks
	case len(ids) == 0:
		return "", invalid("a prompt needs a preset or a list of blocks")
	}
	blocks := make([]prompt.Block, 0, len(ids))
	for _, id := range ids {
		b, err := s.st.Blocks.Get(id)
		if err != nil {
			return "", err
		}
		blocks = append(blocks, b)
	}
	text, err := prompt.Compose(blocks, ref.Vars)
	if err != nil {
		return "", invalid("%v", err)
	}
	return text, nil
}

func (s *Server) renderPrompt(w http.ResponseWriter, r *http.Request) {
	var ref promptRef
	if !decode(w, r, &ref) {
		return
	}
	text, err := s.resolvePrompt(ref)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

// buildParams merges explicit params with the prompts assembled from blocks.
func (s *Server) buildParams(base map[string]any, positive, negative *promptRef) (map[string]any, error) {
	params := make(map[string]any, len(base)+2)
	for k, v := range base {
		params[k] = v
	}
	refs := []struct {
		name string
		ref  *promptRef
	}{{"positive", positive}, {"negative", negative}}
	for _, e := range refs {
		if e.ref == nil {
			continue
		}
		if _, dup := params[e.name]; dup {
			return nil, invalid("%q is set both in params and as a prompt reference", e.name)
		}
		text, err := s.resolvePrompt(*e.ref)
		if err != nil {
			return nil, err
		}
		params[e.name] = text
	}
	return params, nil
}

func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version  int            `json:"version"`
		Params   map[string]any `json:"params"`
		Positive *promptRef     `json:"positive"`
		Negative *promptRef     `json:"negative"`
	}
	if !decode(w, r, &req) {
		return
	}
	params, err := s.buildParams(req.Params, req.Positive, req.Negative)
	if err != nil {
		s.fail(w, err)
		return
	}
	run, err := s.rn.Submit(r.Context(), r.PathValue("slug"), req.Version, params)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

const maxBatch = 64

// batchWorkflow queues several runs of one workflow: the cartesian product of
// the grid values, each repeated count times. Seeds left unset are drawn at
// random for every run, so count > 1 gives different images.
func (s *Server) batchWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version  int              `json:"version"`
		Params   map[string]any   `json:"params"`
		Positive *promptRef       `json:"positive"`
		Negative *promptRef       `json:"negative"`
		Grid     map[string][]any `json:"grid"`
		Count    int              `json:"count"`
	}
	if !decode(w, r, &req) {
		return
	}
	count := req.Count
	if count == 0 {
		count = 1
	}
	if count < 0 {
		s.fail(w, invalid("count must be positive"))
		return
	}
	base, err := s.buildParams(req.Params, req.Positive, req.Negative)
	if err != nil {
		s.fail(w, err)
		return
	}

	names := make([]string, 0, len(req.Grid))
	for k := range req.Grid {
		names = append(names, k)
	}
	sort.Strings(names)
	combos := []map[string]any{{}}
	for _, k := range names {
		values := req.Grid[k]
		if len(values) == 0 {
			s.fail(w, invalid("grid %q has no values", k))
			return
		}
		if _, dup := base[k]; dup {
			s.fail(w, invalid("%q is set both in the grid and in params or prompts", k))
			return
		}
		if len(combos)*len(values) > maxBatch {
			s.fail(w, invalid("a batch is limited to %d runs", maxBatch))
			return
		}
		next := make([]map[string]any, 0, len(combos)*len(values))
		for _, c := range combos {
			for _, v := range values {
				m := make(map[string]any, len(c)+1)
				for ck, cv := range c {
					m[ck] = cv
				}
				m[k] = v
				next = append(next, m)
			}
		}
		combos = next
	}
	if total := len(combos) * count; total > maxBatch {
		s.fail(w, invalid("a batch is limited to %d runs (this one would make %d)", maxBatch, total))
		return
	}

	batchID := store.NewRunID()
	runs := make([]store.Run, 0, len(combos)*count)
	for _, combo := range combos {
		for i := 0; i < count; i++ {
			params := make(map[string]any, len(base)+len(combo))
			for k, v := range base {
				params[k] = v
			}
			for k, v := range combo {
				params[k] = v
			}
			run, err := s.rn.SubmitWith(r.Context(), r.PathValue("slug"), req.Version, params, runner.Meta{BatchID: batchID})
			if err != nil {
				if len(runs) == 0 {
					s.fail(w, err)
					return
				}
				// Part of the batch is already queued: report both.
				s.log.Warn("batch stopped early", "batch", batchID, "submitted", len(runs), "err", err)
				writeJSON(w, http.StatusAccepted, map[string]any{
					"batch_id": batchID, "count": len(runs), "runs": runs,
					"error": err.Error(), "failed_at": len(runs),
				})
				return
			}
			runs = append(runs, run)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"batch_id": batchID, "count": len(runs), "runs": runs})
}

// Runs

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{
		Workflow: q.Get("workflow"),
		Status:   q.Get("status"),
		Batch:    q.Get("batch"),
		Tag:      strings.ToLower(q.Get("tag")),
		Limit:    50,
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		f.Limit = n
	}
	switch f.Status {
	case "", store.StatusQueued, store.StatusRunning, store.StatusSucceeded, store.StatusFailed, store.StatusCancelled:
	default:
		writeError(w, http.StatusBadRequest, "status must be queued, running, succeeded, failed or cancelled")
		return
	}
	if v := q.Get("favorite"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "favorite must be true or false")
			return
		}
		f.Favorite = b
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be an RFC 3339 timestamp, e.g. 2026-01-31T00:00:00Z")
			return
		}
		f.Since = t
	}
	runs, err := s.st.FilterRuns(f)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.rn.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// replayRun queues the same workflow version with the exact parameters of an
// earlier run (seed included). params overrides single values; new_seed draws
// a fresh seed instead of reusing the old one.
func (s *Server) replayRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Params  map[string]any `json:"params"`
		NewSeed bool           `json:"new_seed"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	orig, err := s.st.GetRun(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	wf, err := s.st.GetWorkflow(orig.Workflow)
	if err != nil {
		s.fail(w, err)
		return
	}
	v, ok := wf.Get(orig.Version)
	if !ok {
		s.fail(w, fmt.Errorf("%w: workflow %q has no version %d", store.ErrNotFound, wf.Slug, orig.Version))
		return
	}
	params := make(map[string]any, len(orig.Params))
	for k, val := range orig.Params {
		params[k] = val
	}
	if req.NewSeed {
		for _, b := range v.Bindings {
			if b.Type == workflow.TypeSeed {
				params[b.Name] = -1
			}
		}
	}
	for k, val := range req.Params {
		params[k] = val
	}
	run, err := s.rn.SubmitWith(r.Context(), orig.Workflow, orig.Version, params, runner.Meta{ReplayOf: orig.ID})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

// patchRun edits the user-owned fields of a finished run.
func (s *Server) patchRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tags     *[]string `json:"tags"`
		Favorite *bool     `json:"favorite"`
		Note     *string   `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	run, err := s.st.UpdateRun(r.PathValue("id"), func(run *store.Run) error {
		if !run.Finished() {
			return fmt.Errorf("%w: run %s is still %s; annotate it once it has finished", store.ErrExists, run.ID, run.Status)
		}
		if req.Tags != nil {
			tags, err := store.NormalizeTags(*req.Tags)
			if err != nil {
				return invalid("%v", err)
			}
			run.Tags = tags
		}
		if req.Favorite != nil {
			run.Favorite = *req.Favorite
		}
		if req.Note != nil {
			if len(*req.Note) > 2000 {
				return invalid("note is limited to 2000 bytes")
			}
			run.Note = *req.Note
		}
		return nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.GetRun(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getOutput(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.GetRun(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	name := r.PathValue("name")
	for _, o := range run.Outputs {
		if o.Name == name && name == filepath.Base(name) {
			http.ServeFile(w, r, filepath.Join(s.st.RunDir(run.ID), name))
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such output")
}

// blockNotInUse refuses to delete a block that a preset still references.
func (s *Server) blockNotInUse(id string) error {
	presets, err := s.st.Presets.List()
	if err != nil {
		return err
	}
	var users []string
	for _, p := range presets {
		for _, b := range p.Blocks {
			if b == id {
				users = append(users, p.ID)
				break
			}
		}
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: block %q is used by presets: %s", store.ErrExists, id, strings.Join(users, ", "))
	}
	return nil
}

// Generic CRUD for blocks and presets

type entity[T any] struct {
	table        *store.Table[T]
	id           func(T) string
	setID        func(*T, string)
	check        func(T) error
	beforeDelete func(id string) error
}

func mountCRUD[T any](s *Server, mux *http.ServeMux, path string, e entity[T]) {
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		items, err := e.table.List()
		if err != nil {
			s.fail(w, err)
			return
		}
		if items == nil {
			items = []T{}
		}
		writeJSON(w, http.StatusOK, items)
	})

	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var v T
		if !decode(w, r, &v) {
			return
		}
		id := e.id(v)
		if !store.ValidID(id) {
			s.fail(w, invalid("id must be lowercase letters, digits, - or _ (got %q)", id))
			return
		}
		if err := e.check(v); err != nil {
			s.fail(w, invalid("%v", err))
			return
		}
		if _, err := e.table.Get(id); err == nil {
			s.fail(w, fmt.Errorf("%w: %q", store.ErrExists, id))
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, err)
			return
		}
		if err := e.table.Put(id, v); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, v)
	})

	mux.HandleFunc("GET "+path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := e.table.Get(r.PathValue("id"))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	})

	mux.HandleFunc("PUT "+path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := e.table.Get(id); err != nil {
			s.fail(w, err)
			return
		}
		var v T
		if !decode(w, r, &v) {
			return
		}
		if got := e.id(v); got != "" && got != id {
			s.fail(w, invalid("id in body (%q) does not match the URL (%q)", got, id))
			return
		}
		e.setID(&v, id)
		if err := e.check(v); err != nil {
			s.fail(w, invalid("%v", err))
			return
		}
		if err := e.table.Put(id, v); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	})

	mux.HandleFunc("DELETE "+path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if e.beforeDelete != nil {
			if err := e.beforeDelete(id); err != nil {
				s.fail(w, err)
				return
			}
		}
		if err := e.table.Delete(id); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
