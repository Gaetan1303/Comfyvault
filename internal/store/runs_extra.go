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

// FilterRuns returns the newest runs first that match f.
func (s *Store) FilterRuns(f RunFilter) ([]Run, error) {
	all, err := s.ListRuns(f.Workflow, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(all))
	for _, r := range all {
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		if f.Batch != "" && r.BatchID != f.Batch {
			continue
		}
		if f.Favorite && !r.Favorite {
			continue
		}
		if !f.Since.IsZero() && r.CreatedAt.Before(f.Since) {
			continue
		}
		if f.Tag != "" && !hasTag(r.Tags, f.Tag) {
			continue
		}
		out = append(out, r)
		if f.Limit > 0 && len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// UpdateRun reads a run, lets fn modify it and saves it, all under the store
// lock. Only use it on finished runs: the runner writes runs that are still
// in flight without going through this lock.
func (s *Store) UpdateRun(id string, fn func(*Run) error) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.GetRun(id)
	if err != nil {
		return r, err
	}
	if err := fn(&r); err != nil {
		return r, err
	}
	return r, s.PutRun(r)
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

// Prune deletes finished runs created before cutoff, with their output
// files. Favorites are kept unless includeFavorites is set. With dryRun
// nothing is deleted; the result describes what would be. Runs still in
// flight are never touched.
func (s *Store) Prune(cutoff time.Time, includeFavorites, dryRun bool) (removed []Run, freed int64, err error) {
	runs, err := s.ListRuns("", 0)
	if err != nil {
		return nil, 0, err
	}
	for _, r := range runs {
		if !r.Finished() || !r.CreatedAt.Before(cutoff) || (r.Favorite && !includeFavorites) {
			continue
		}
		size := dirSize(s.RunDir(r.ID))
		if !dryRun {
			if err := os.RemoveAll(s.RunDir(r.ID)); err != nil {
				return removed, freed, err
			}
			if err := os.Remove(s.runPath(r.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, freed, err
			}
		}
		removed = append(removed, r)
		freed += size
	}
	return removed, freed, nil
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
