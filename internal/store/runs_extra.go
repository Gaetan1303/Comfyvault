package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RunFilter selects runs for FilterRuns. Zero fields match everything.
type RunFilter struct {
	Workflow string
	Status   string
	Batch    string
	Tag      string
	Favorite bool // only favorites when true
	Since    time.Time
	Limit    int
}

// FilterRuns returns the newest runs first that match f, reading only as many
// run files as it needs to fill the limit.
func (s *Store) FilterRuns(f RunFilter) ([]Run, error) {
	out := []Run{}
	err := s.walkRuns(func(r Run) bool {
		switch {
		case f.Workflow != "" && r.Workflow != f.Workflow,
			f.Status != "" && r.Status != f.Status,
			f.Batch != "" && r.BatchID != f.Batch,
			f.Favorite && !r.Favorite,
			!f.Since.IsZero() && r.CreatedAt.Before(f.Since),
			f.Tag != "" && !hasTag(r.Tags, f.Tag):
			return true
		}
		out = append(out, r)
		return f.Limit <= 0 || len(out) < f.Limit
	})
	return out, err
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// UpdateRun reads a finished run, lets fn modify it and saves it, all under
// the store lock. Runs still in flight belong to the runner and are refused
// with ErrExists, so a user edit can never overwrite a status change.
func (s *Store) UpdateRun(id string, fn func(*Run) error) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.GetRun(id)
	if err != nil {
		return r, err
	}
	if !r.Finished() {
		return r, fmt.Errorf("%w: run %s is still %s; edit it once it has finished", ErrExists, r.ID, r.Status)
	}
	if err := fn(&r); err != nil {
		return r, err
	}
	return r, s.putRun(r)
}

var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,31}$`)

const maxTags = 20

// NormalizeTags lowercases and trims tags, drops duplicates (keeping the
// first occurrence) and rejects anything that is not a short slug.
func NormalizeTags(in []string) ([]string, error) {
	if len(in) > maxTags {
		return nil, fmt.Errorf("at most %d tags", maxTags)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if !tagRe.MatchString(t) {
			return nil, fmt.Errorf("tag %q must match %s", t, tagRe)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

func prunable(r Run, cutoff time.Time, includeFavorites bool) bool {
	return r.Finished() && r.CreatedAt.Before(cutoff) && (includeFavorites || !r.Favorite)
}

// Prune deletes finished runs created before cutoff, with their output
// files. Favorites are kept unless includeFavorites is set. With dryRun
// nothing is deleted; the result describes what would be. Runs still in
// flight are never touched. Every run is checked again under the store lock
// right before it is removed, so an edit or a status change that lands in
// between wins over the pruning.
func (s *Store) Prune(cutoff time.Time, includeFavorites, dryRun bool) (removed []Run, freed int64, err error) {
	runs, err := s.ListRuns("", 0)
	if err != nil {
		return nil, 0, err
	}
	for _, r := range runs {
		if !prunable(r, cutoff, includeFavorites) {
			continue
		}
		size := dirSize(s.RunDir(r.ID))
		if !dryRun {
			gone, err := s.removeIf(r.ID, func(cur Run) bool { return prunable(cur, cutoff, includeFavorites) })
			if err != nil {
				return removed, freed, err
			}
			if !gone {
				continue
			}
		}
		removed = append(removed, r)
		freed += size
	}
	return removed, freed, nil
}

// removeIf deletes a run and its outputs when ok approves the stored version.
// The run file goes first: a crash in between leaves an invisible output
// folder rather than a run that points at missing images.
func (s *Store) removeIf(id string, ok func(Run) bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.GetRun(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !ok(cur) {
		return false, nil
	}
	if err := os.Remove(s.runPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, os.RemoveAll(s.RunDir(id))
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
