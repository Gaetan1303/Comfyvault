// Package api exposes comfyvault over HTTP/JSON.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
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
	params := map[string]any{}
	for k, v := range req.Params {
		params[k] = v
	}
	for name, ref := range map[string]*promptRef{"positive": req.Positive, "negative": req.Negative} {
		if ref == nil {
			continue
		}
		if _, dup := params[name]; dup {
			s.fail(w, invalid("%q is set both in params and as a prompt reference", name))
			return
		}
		text, err := s.resolvePrompt(*ref)
		if err != nil {
			s.fail(w, err)
			return
		}
		params[name] = text
	}
	run, err := s.rn.Submit(r.Context(), r.PathValue("slug"), req.Version, params)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

// Runs

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	runs, err := s.st.ListRuns(r.URL.Query().Get("workflow"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
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
