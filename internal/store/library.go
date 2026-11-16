package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/prompt"
	"github.com/Gaetan1303/comfyvault/internal/workflow"
)

// UpdateWorkflowMeta renames a workflow or changes its description. Nil
// arguments are left unchanged. Versions are never touched.
func (s *Store) UpdateWorkflowMeta(slug string, name, desc *string) (*Workflow, error) {
	if !ValidID(slug) {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, err := s.readWorkflow(slug)
	if err != nil {
		return nil, err
	}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" {
			return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalid)
		}
		w.Name = n
	}
	if desc != nil {
		w.Description = strings.TrimSpace(*desc)
	}
	if err := writeJSON(s.workflowPath(slug), w); err != nil {
		return nil, err
	}
	return w, nil
}

// DeleteWorkflow removes a workflow with all its versions. It refuses while
// runs still refer to it, since those could no longer be replayed.
func (s *Store) DeleteWorkflow(slug string) error {
	if !ValidID(slug) {
		return fmt.Errorf("%w: workflow %q", ErrNotFound, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.readWorkflow(slug); err != nil {
		return err
	}
	runs, err := s.ListRuns(slug, 1)
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		return fmt.Errorf("%w: workflow %q still has runs; delete them first", ErrExists, slug)
	}
	return os.Remove(s.workflowPath(slug))
}

// DeleteRun removes a finished run and its output files.
func (s *Store) DeleteRun(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.GetRun(id)
	if err != nil {
		return err
	}
	if !r.Finished() {
		return fmt.Errorf("%w: run %s is still %s; cancel it first", ErrExists, id, r.Status)
	}
	if err := os.Remove(s.runPath(id)); err != nil {
		return err
	}
	return os.RemoveAll(s.RunDir(id))
}

// Stats summarises the library.
type Stats struct {
	Workflows    int            `json:"workflows"`
	Versions     int            `json:"versions"`
	Blocks       int            `json:"blocks"`
	Presets      int            `json:"presets"`
	Runs         int            `json:"runs"`
	RunsByStatus map[string]int `json:"runs_by_status"`
	Favorites    int            `json:"favorites"`
	OutputBytes  int64          `json:"output_bytes"`
}

func (s *Store) Stats() (Stats, error) {
	st := Stats{RunsByStatus: map[string]int{}}
	wfs, err := s.ListWorkflows()
	if err != nil {
		return st, err
	}
	st.Workflows = len(wfs)
	for _, w := range wfs {
		st.Versions += len(w.Versions)
	}
	blocks, err := s.Blocks.List()
	if err != nil {
		return st, err
	}
	presets, err := s.Presets.List()
	if err != nil {
		return st, err
	}
	st.Blocks, st.Presets = len(blocks), len(presets)
	runs, err := s.ListRuns("", 0)
	if err != nil {
		return st, err
	}
	st.Runs = len(runs)
	for _, r := range runs {
		st.RunsByStatus[r.Status]++
		if r.Favorite {
			st.Favorites++
		}
	}
	st.OutputBytes = dirSize(filepath.Join(s.dir, "outputs"))
	return st, nil
}

// BundleFormat identifies library export files.
const BundleFormat = "comfyvault-library"

// Bundle is a portable copy of workflows (all versions), blocks and presets.
// Runs and their images are not included.
type Bundle struct {
	Format     string          `json:"format"`
	ExportedAt time.Time       `json:"exported_at"`
	Workflows  []Workflow      `json:"workflows"`
	Blocks     []prompt.Block  `json:"blocks"`
	Presets    []prompt.Preset `json:"presets"`
}

func (s *Store) Export() (Bundle, error) {
	wfs, err := s.ListWorkflows()
	if err != nil {
		return Bundle{}, err
	}
	if wfs == nil {
		wfs = []Workflow{}
	}
	blocks, err := s.Blocks.List()
	if err != nil {
		return Bundle{}, err
	}
	presets, err := s.Presets.List()
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Format: BundleFormat, ExportedAt: time.Now().UTC(), Workflows: wfs, Blocks: blocks, Presets: presets}, nil
}

// ImportReport says what an import changed.
type ImportReport struct {
	WorkflowVersions int      `json:"workflow_versions"`
	Blocks           int      `json:"blocks"`
	Presets          int      `json:"presets"`
	Skipped          []string `json:"skipped"`
	Errors           []string `json:"errors"`
}

// Import merges a bundle into the library. Workflow versions identical to
// the latest stored one are ignored, different ones are appended as new
// versions. Existing blocks and presets are kept unless overwrite is set.
// Problems with single items are reported, not fatal.
func (s *Store) Import(b Bundle, overwrite bool) (ImportReport, error) {
	rep := ImportReport{Skipped: []string{}, Errors: []string{}}
	if b.Format != BundleFormat {
		return rep, fmt.Errorf("%w: not a comfyvault library export (format %q)", ErrInvalid, b.Format)
	}
	for _, w := range b.Workflows {
		for _, v := range w.Versions {
			bindings := v.Bindings
			if bindings == nil {
				bindings = []workflow.Binding{}
			}
			_, _, created, err := s.PublishWorkflow(w.Slug, w.Name, w.Description, v.API, bindings)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("workflow %s v%d: %v", w.Slug, v.Version, err))
				break
			}
			if created {
				rep.WorkflowVersions++
			}
		}
	}
	for _, bl := range b.Blocks {
		if !ValidID(bl.ID) || strings.TrimSpace(bl.Text) == "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("block %q: invalid id or empty text", bl.ID))
			continue
		}
		if _, err := s.Blocks.Get(bl.ID); err == nil && !overwrite {
			rep.Skipped = append(rep.Skipped, "block "+bl.ID)
			continue
		}
		if err := s.Blocks.Put(bl.ID, bl); err != nil {
			return rep, err
		}
		rep.Blocks++
	}
	for _, p := range b.Presets {
		if !ValidID(p.ID) || len(p.Blocks) == 0 {
			rep.Errors = append(rep.Errors, fmt.Sprintf("preset %q: invalid id or no blocks", p.ID))
			continue
		}
		missing := ""
		for _, id := range p.Blocks {
			if _, err := s.Blocks.Get(id); err != nil {
				missing = id
				break
			}
		}
		if missing != "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("preset %s: unknown block %q", p.ID, missing))
			continue
		}
		if _, err := s.Presets.Get(p.ID); err == nil && !overwrite {
			rep.Skipped = append(rep.Skipped, "preset "+p.ID)
			continue
		}
		if err := s.Presets.Put(p.ID, p); err != nil {
			return rep, err
		}
		rep.Presets++
	}
	return rep, nil
}
