package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Table is a small keyed collection persisted as one JSON file.
type Table[T any] struct {
	path string
	mu   sync.Mutex
}

func (t *Table[T]) load() (map[string]T, error) {
	m := map[string]T{}
	raw, err := os.ReadFile(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", t.path, err)
	}
	return m, nil
}

func (t *Table[T]) Put(id string, v T) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return err
	}
	m[id] = v
	return writeJSON(t.path, m)
}

func (t *Table[T]) Get(id string) (T, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var zero T
	m, err := t.load()
	if err != nil {
		return zero, err
	}
	v, ok := m[id]
	if !ok {
		return zero, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return v, nil
}

// GetMany returns the values for ids, in that order, reading the file once.
func (t *Table[T]) GetMany(ids []string) ([]T, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(ids))
	for _, id := range ids {
		v, ok := m[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		out = append(out, v)
	}
	return out, nil
}

func (t *Table[T]) List() ([]T, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out, nil
}

func (t *Table[T]) Delete(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return err
	}
	if _, ok := m[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(m, id)
	return writeJSON(t.path, m)
}

// writeJSON replaces path atomically so a crash never leaves a half-written file.
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes the rename itself durable. It is best effort: some platforms
// cannot open or sync a directory, and the data is already safely written.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
