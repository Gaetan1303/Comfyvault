package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneAndFilter(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	put := func(id, status string, fav bool, at time.Time) {
		t.Helper()
		if err := st.PutRun(Run{ID: id, Workflow: "wf", Status: status, Favorite: fav, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(st.RunDir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.RunDir(id), "img.png"), []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("20200101-000000-aaaaaa", StatusSucceeded, false, old)
	put("20200102-000000-bbbbbb", StatusSucceeded, true, old)
	put("20200103-000000-cccccc", StatusRunning, false, old)
	put("20200104-000000-dddddd", StatusCancelled, false, old)
	recent := NewRunID()
	put(recent, StatusSucceeded, false, time.Now().UTC())

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	removed, freed, err := st.Prune(cutoff, false, true)
	if err != nil || len(removed) != 2 || freed != 10 {
		t.Fatalf("dry run: removed=%d freed=%d err=%v", len(removed), freed, err)
	}
	if _, err := st.GetRun("20200101-000000-aaaaaa"); err != nil {
		t.Fatal("a dry run must not delete anything")
	}

	if _, _, err := st.Prune(cutoff, false, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"20200101-000000-aaaaaa", "20200104-000000-dddddd"} {
		if _, err := st.GetRun(id); err == nil {
			t.Errorf("%s should have been pruned", id)
		}
		if _, err := os.Stat(st.RunDir(id)); err == nil {
			t.Errorf("outputs of %s should have been pruned", id)
		}
	}
	for _, id := range []string{"20200102-000000-bbbbbb", "20200103-000000-cccccc", recent} {
		if _, err := st.GetRun(id); err != nil {
			t.Errorf("%s must survive (favorite, in flight or recent): %v", id, err)
		}
	}

	favs, err := st.FilterRuns(RunFilter{Favorite: true})
	if err != nil || len(favs) != 1 || favs[0].ID != "20200102-000000-bbbbbb" {
		t.Fatalf("favorite filter: %+v err=%v", favs, err)
	}
	if n, _ := st.FilterRuns(RunFilter{Status: StatusRunning}); len(n) != 1 {
		t.Fatalf("status filter: %+v", n)
	}
	if n, _ := st.FilterRuns(RunFilter{Limit: 1}); len(n) != 1 || n[0].ID != recent {
		t.Fatalf("limit should keep the newest run: %+v", n)
	}
}

func TestNormalizeTags(t *testing.T) {
	got, err := NormalizeTags([]string{" Portrait ", "portrait", "keep-1"})
	if err != nil || len(got) != 2 || got[0] != "portrait" || got[1] != "keep-1" {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range [][]string{{""}, {"has space"}, {"-lead"}, {"é"}} {
		if _, err := NormalizeTags(bad); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}
