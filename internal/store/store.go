// Package store persists workflows, prompt blocks, presets and runs as plain
// JSON files under one data directory.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/prompt"
	"github.com/Gaetan1303/comfyvault/internal/workflow"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrExists   = errors.New("already exists")
)

var (
	idRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	runIDRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)
)

// ValidID reports whether s is usable as a workflow slug, block id or preset id.
func ValidID(s string) bool { return idRe.MatchString(s) }

type Store struct {
	dir     string
	Blocks  *Table[prompt.Block]
	Presets *Table[prompt.Preset]
	mu      sync.Mutex // workflows and runs
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{"workflows", "runs", "outputs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{
		dir:     dir,
		Blocks:  &Table[prompt.Block]{path: filepath.Join(dir, "blocks.json")},
		Presets: &Table[prompt.Preset]{path: filepath.Join(dir, "presets.json")},
	}, nil
}

// Workflows

type Version struct {
	Version   int                `json:"version"`
	Hash      string             `json:"hash"`
	CreatedAt time.Time          `json:"created_at"`
	API       json.RawMessage    `json:"api"`
	Bindings  []workflow.Binding `json:"bindings"`
}

type Workflow struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Versions    []Version `json:"versions"`
}

func (w *Workflow) Latest() Version { return w.Versions[len(w.Versions)-1] }

// Get returns version n, or the latest one when n is 0.
func (w *Workflow) Get(n int) (Version, bool) {
	if n == 0 {
		return w.Latest(), true
	}
	for _, v := range w.Versions {
		if v.Version == n {
			return v, true
		}
	}
	return Version{}, false
}

func (s *Store) workflowPath(slug string) string {
	return filepath.Join(s.dir, "workflows", slug+".json")
}

func (s *Store) GetWorkflow(slug string) (*Workflow, error) {
	if !ValidID(slug) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readWorkflow(slug)
}

func (s *Store) readWorkflow(slug string) (*Workflow, error) {
	raw, err := os.ReadFile(s.workflowPath(slug))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, slug)
	}
	if err != nil {
		return nil, err
	}
	var w Workflow
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("workflow %q: %w", slug, err)
	}
	if len(w.Versions) == 0 {
		return nil, fmt.Errorf("workflow %q has no versions", slug)
	}
	return &w, nil
}

func (s *Store) ListWorkflows() ([]Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.dir, "workflows"))
	if err != nil {
		return nil, err
	}
	var out []Workflow
	for _, e := range entries {
		slug, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidID(slug) {
			continue
		}
		w, err := s.readWorkflow(slug)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, nil
}

// PublishWorkflow stores raw (API format) under slug. A nil bindings slice
// means "suggest them"; an empty one means "no parameters". Publishing the
// same graph and bindings again is a no-op; anything else becomes a new
// version. created is false when nothing changed.
func (s *Store) PublishWorkflow(slug, name, desc string, raw []byte, bindings []workflow.Binding) (w *Workflow, v Version, created bool, err error) {
	if !ValidID(slug) {
		return nil, v, false, fmt.Errorf("%w: slug must match %s", ErrInvalid, idRe)
	}
	if len(raw) == 0 {
		return nil, v, false, fmt.Errorf("%w: workflow JSON is missing", ErrInvalid)
	}
	g, err := workflow.Parse(raw)
	if err != nil {
		return nil, v, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if bindings == nil {
		bindings = workflow.Suggest(g)
	}
	if err := g.ValidateBindings(bindings); err != nil {
		return nil, v, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	canonical, err := json.Marshal(g)
	if err != nil {
		return nil, v, false, err
	}
	bjson, err := json.Marshal(bindings)
	if err != nil {
		return nil, v, false, err
	}
	sum := sha256.Sum256(append(append([]byte{}, canonical...), bjson...))
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.readWorkflow(slug)
	switch {
	case errors.Is(err, ErrNotFound):
		existing = &Workflow{Slug: slug}
	case err != nil:
		return nil, v, false, err
	}
	if n := len(existing.Versions); n > 0 && existing.Latest().Hash == hash {
		return existing, existing.Latest(), false, nil
	}
	if name != "" {
		existing.Name = name
	}
	if existing.Name == "" {
		existing.Name = slug
	}
	if desc != "" {
		existing.Description = desc
	}
	v = Version{
		Version:   len(existing.Versions) + 1,
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		API:       canonical,
		Bindings:  bindings,
	}
	existing.Versions = append(existing.Versions, v)
	if err := writeJSON(s.workflowPath(slug), existing); err != nil {
		return nil, v, false, err
	}
	return existing, v, true, nil
}

// Runs

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

type Output struct {
	Name      string `json:"name"`
	Node      string `json:"node"`
	Subfolder string `json:"subfolder,omitempty"`
}

type Run struct {
	ID         string         `json:"id"`
	Workflow   string         `json:"workflow"`
	Version    int            `json:"version"`
	Status     string         `json:"status"`
	Params     map[string]any `json:"params"`
	PromptID   string         `json:"prompt_id,omitempty"`
	Error      string         `json:"error,omitempty"`
	Outputs    []Output       `json:"outputs,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
}

func NewRunID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func (s *Store) runPath(id string) string { return filepath.Join(s.dir, "runs", id+".json") }

// RunDir is where the outputs of a run are stored.
func (s *Store) RunDir(id string) string { return filepath.Join(s.dir, "outputs", id) }

func (s *Store) PutRun(r Run) error {
	if !runIDRe.MatchString(r.ID) {
		return fmt.Errorf("%w: bad run id %q", ErrInvalid, r.ID)
	}
	return writeJSON(s.runPath(r.ID), r)
}

func (s *Store) GetRun(id string) (Run, error) {
	var r Run
	if !runIDRe.MatchString(id) {
		return r, fmt.Errorf("%w: run %q", ErrNotFound, id)
	}
	raw, err := os.ReadFile(s.runPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("%w: run %q", ErrNotFound, id)
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(raw, &r)
}

// ListRuns returns the newest runs first, optionally filtered by workflow.
func (s *Store) ListRuns(workflowSlug string, limit int) ([]Run, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		return nil, err
	}
	var out []Run
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !runIDRe.MatchString(id) {
			continue
		}
		r, err := s.GetRun(id)
		if err != nil {
			return nil, err
		}
		if workflowSlug == "" || r.Workflow == workflowSlug {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
