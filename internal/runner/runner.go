// Package runner turns a stored workflow plus parameters into a ComfyUI job
// and follows it until the outputs are on disk.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/comfy"
	"github.com/Gaetan1303/comfyvault/internal/store"
	"github.com/Gaetan1303/comfyvault/internal/workflow"
)

// ErrUpstream marks failures to reach ComfyUI, as opposed to bad requests.
var ErrUpstream = errors.New("comfyui unreachable")

const maxConsecutiveFailures = 20

type Runner struct {
	st      *store.Store
	cl      *comfy.Client
	poll    time.Duration
	timeout time.Duration
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(st *store.Store, cl *comfy.Client, poll, timeout time.Duration, log *slog.Logger) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{st: st, cl: cl, poll: poll, timeout: timeout, log: log, ctx: ctx, cancel: cancel}
}

// Close stops all follow-up goroutines. Runs still in flight stay recorded as
// queued or running and are picked up again by Recover on the next start.
func (r *Runner) Close() {
	r.cancel()
	r.wg.Wait()
}

// Recover resumes runs that were in flight when the process last stopped.
func (r *Runner) Recover() error {
	runs, err := r.st.ListRuns("", 0)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status != store.StatusQueued && run.Status != store.StatusRunning {
			continue
		}
		if run.PromptID == "" {
			r.fail(&run, "interrupted before it reached ComfyUI")
			continue
		}
		r.log.Info("resuming run", "run", run.ID, "prompt_id", run.PromptID)
		r.spawn(run)
	}
	return nil
}

// Submit injects params into the workflow, queues it in ComfyUI and starts
// following it in the background. Validation errors from ComfyUI (missing
// model, bad value...) come back synchronously as *comfy.APIError.
func (r *Runner) Submit(ctx context.Context, slug string, version int, params map[string]any) (store.Run, error) {
	wf, err := r.st.GetWorkflow(slug)
	if err != nil {
		return store.Run{}, err
	}
	v, ok := wf.Get(version)
	if !ok {
		return store.Run{}, fmt.Errorf("%w: workflow %q has no version %d", store.ErrNotFound, slug, version)
	}
	g, err := workflow.Parse(v.API)
	if err != nil {
		return store.Run{}, err
	}
	g, resolved, err := workflow.Apply(g, v.Bindings, params)
	if err != nil {
		return store.Run{}, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	promptID, err := r.cl.Queue(ctx, g)
	if err != nil {
		var ae *comfy.APIError
		if errors.As(err, &ae) {
			return store.Run{}, err
		}
		return store.Run{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	run := store.Run{
		ID:        store.NewRunID(),
		Workflow:  slug,
		Version:   v.Version,
		Status:    store.StatusQueued,
		Params:    resolved,
		PromptID:  promptID,
		CreatedAt: time.Now().UTC(),
	}
	if err := r.st.PutRun(run); err != nil {
		return store.Run{}, err
	}
	r.spawn(run)
	return run, nil
}

func (r *Runner) spawn(run store.Run) {
	r.wg.Add(1)
	go r.follow(run)
}

func (r *Runner) follow(run store.Run) {
	defer r.wg.Done()
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	tick := time.NewTicker(r.poll)
	defer tick.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			if r.ctx.Err() == nil {
				r.fail(&run, fmt.Sprintf("timed out after %s", r.timeout))
			}
			return
		case <-tick.C:
		}

		entry, err := r.cl.History(ctx, run.PromptID)
		if err != nil {
			failures++
			r.log.Warn("history poll failed", "run", run.ID, "err", err, "consecutive", failures)
			if failures >= maxConsecutiveFailures {
				r.fail(&run, "lost contact with ComfyUI: "+err.Error())
				return
			}
			continue
		}
		failures = 0

		if entry == nil {
			if run.Status == store.StatusQueued {
				if pos, err := r.cl.Position(ctx, run.PromptID); err == nil && pos == "running" {
					run.Status = store.StatusRunning
					if err := r.st.PutRun(run); err != nil {
						r.log.Error("saving run", "run", run.ID, "err", err)
					}
				}
			}
			continue
		}

		switch {
		case entry.Status.StatusStr == "error":
			r.fail(&run, entry.ErrorMessage())
			return
		case entry.Status.Completed:
			if err := r.collect(ctx, &run, entry); err != nil {
				if r.ctx.Err() != nil {
					return
				}
				r.fail(&run, "fetching outputs: "+err.Error())
				return
			}
			now := time.Now().UTC()
			run.Status = store.StatusSucceeded
			run.FinishedAt = &now
			if err := r.st.PutRun(run); err != nil {
				r.log.Error("saving run", "run", run.ID, "err", err)
			}
			return
		}
	}
}

func (r *Runner) collect(ctx context.Context, run *store.Run, entry *comfy.HistoryEntry) error {
	dir := r.st.RunDir(run.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	used := map[string]bool{}
	run.Outputs = nil
	for _, f := range entry.Files() {
		if f.Type != "output" {
			continue
		}
		base := filepath.Base(f.Filename)
		name := base
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s_%d_%s", f.Node, i, base)
		}
		used[name] = true
		if err := r.download(ctx, f.FileRef, filepath.Join(dir, name)); err != nil {
			return err
		}
		run.Outputs = append(run.Outputs, store.Output{Name: name, Node: f.Node, Subfolder: f.Subfolder})
	}
	return nil
}

func (r *Runner) download(ctx context.Context, ref comfy.FileRef, dest string) error {
	rc, err := r.cl.View(ctx, ref)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".dl-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func (r *Runner) fail(run *store.Run, msg string) {
	now := time.Now().UTC()
	run.Status = store.StatusFailed
	run.Error = msg
	run.FinishedAt = &now
	if err := r.st.PutRun(*run); err != nil {
		r.log.Error("saving run", "run", run.ID, "err", err)
	}
	r.log.Warn("run failed", "run", run.ID, "reason", msg)
}
