package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/store"
)

const maxImport = 64 << 20

func (s *Server) patchWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !decode(w, r, &req) {
		return
	}
	wf, err := s.st.UpdateWorkflowMeta(r.PathValue("slug"), req.Name, req.Description)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"slug": wf.Slug, "name": wf.Name, "description": wf.Description})
}

func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteWorkflow(r.PathValue("slug")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteRun(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) libraryStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) libraryExport(w http.ResponseWriter, r *http.Request) {
	b, err := s.st.Export()
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="comfyvault-library.json"`)
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) libraryImport(w http.ResponseWriter, r *http.Request) {
	var b store.Bundle
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImport))
	if err := dec.Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid library file: "+err.Error())
		return
	}
	if !noTrailing(w, dec) {
		return
	}
	rep, err := s.st.Import(b, r.URL.Query().Get("overwrite") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// libraryPrune deletes finished runs older than a number of days.
func (s *Server) libraryPrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OlderThanDays    int  `json:"older_than_days"`
		IncludeFavorites bool `json:"include_favorites"`
		DryRun           bool `json:"dry_run"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.OlderThanDays < 1 {
		s.fail(w, invalid("older_than_days must be at least 1"))
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(req.OlderThanDays) * 24 * time.Hour)
	removed, freed, err := s.st.Prune(cutoff, req.IncludeFavorites, req.DryRun)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dry_run": req.DryRun, "runs": len(removed), "freed_bytes": freed})
}
