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

const (
	// lostAfter is how long ComfyUI may stay unreachable before a run is given
	// up. Polling backs off up to maxBackoff meanwhile, so a restart of ComfyUI
	// or a slow /history does not condemn jobs that are still queued there.
	lostAfter  = 90 * time.Second
	maxBackoff = 15 * time.Second
	// maxOutputBytes caps a single downloaded file.
	maxOutputBytes = 1 << 30
)

// cancelReq carries the caller's context to the goroutine doing the work, so
// the deadline the caller sees is the deadline that really applies.
type cancelReq struct {
	ctx   context.Context
	reply chan error
}

type Runner struct {
	st      *store.Store
	cl      *comfy.Client
	poll    time.Duration
	timeout time.Duration
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	cancels map[string]chan cancelReq // run id -> cancellation requests
}

func New(st *store.Store, cl *comfy.Client, poll, timeout time.Duration, log *slog.Logger) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{st: st, cl: cl, poll: poll, timeout: timeout, log: log, ctx: ctx, cancel: cancel,
		cancels: map[string]chan cancelReq{}}
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

// Meta records where a run comes from.
type Meta struct {
	ReplayOf string
	BatchID  string
}

// Submit injects params into the workflow, queues it in ComfyUI and starts
// following it in the background. Validation errors from ComfyUI (missing
// model, bad value...) come back synchronously as *comfy.APIError.
func (r *Runner) Submit(ctx context.Context, slug string, version int, params map[string]any) (store.Run, error) {
	return r.SubmitWith(ctx, slug, version, params, Meta{})
}

// SubmitWith is Submit with provenance information attached to the run.
func (r *Runner) SubmitWith(ctx context.Context, slug string, version int, params map[string]any, meta Meta) (store.Run, error) {
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
	if err := ctx.Err(); err != nil {
		return store.Run{}, err
	}
	// The request context only guards what happens before the job is handed to
	// ComfyUI. Once Queue is in flight a client that hangs up must not leave a
	// job running that nobody recorded, so it is detached from cancellation.
	qctx := context.WithoutCancel(ctx)
	promptID, err := r.cl.Queue(qctx, g)
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
		ReplayOf:  meta.ReplayOf,
		BatchID:   meta.BatchID,
	}
	if err := r.st.PutRun(run); err != nil {
		if derr := r.cl.DeleteQueued(qctx, promptID); derr != nil {
			r.log.Error("job accepted by ComfyUI but not recorded", "prompt_id", promptID, "err", err, "dequeue_err", derr)
		}
		return store.Run{}, err
	}
	r.spawn(run)
	return run, nil
}

func (r *Runner) spawn(run store.Run) {
	ch := make(chan cancelReq, 1)
	r.mu.Lock()
	r.cancels[run.ID] = ch
	r.mu.Unlock()
	r.wg.Add(1)
	go r.follow(run, ch)
}

func (r *Runner) unregister(id string) {
	r.mu.Lock()
	delete(r.cancels, id)
	r.mu.Unlock()
}

// Cancel stops a queued or running run: it is removed from ComfyUI's queue
// or interrupted, and recorded as cancelled. The follow-up goroutine does the
// work, under the context given here, so that only one goroutine ever writes
// a run that is in flight and the 15 s limit bounds the work itself, not just
// the wait. store.ErrExists is returned when the run has already finished.
func (r *Runner) Cancel(ctx context.Context, id string) (store.Run, error) {
	r.mu.Lock()
	ch, ok := r.cancels[id]
	r.mu.Unlock()
	if !ok {
		run, err := r.st.GetRun(id)
		if err != nil {
			return run, err
		}
		if run.Finished() {
			return run, fmt.Errorf("%w: run %s is already %s", store.ErrExists, id, run.Status)
		}
		return run, fmt.Errorf("%w: run %s is not being followed by this process", store.ErrExists, id)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req := cancelReq{ctx: ctx, reply: make(chan error, 1)}
	select {
	case ch <- req:
	default:
		return store.Run{}, fmt.Errorf("%w: a cancellation of run %s is already in progress", store.ErrExists, id)
	}
	select {
	case err := <-req.reply:
		if err != nil {
			return store.Run{}, err
		}
	case <-ctx.Done():
		return store.Run{}, fmt.Errorf("cancelling run %s: %w", id, ctx.Err())
	case <-r.ctx.Done():
		return store.Run{}, errors.New("service is shutting down")
	}
	return r.st.GetRun(id)
}

// upstream classifies an error from ComfyUI: timeouts and cancellations keep
// their identity (the API answers 504 for them), the rest is ErrUpstream.
func upstream(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrUpstream, err)
}

func (r *Runner) doCancel(ctx context.Context, run *store.Run) error {
	pos, err := r.cl.Position(ctx, run.PromptID)
	if err != nil {
		return upstream(err)
	}
	switch pos {
	case "pending":
		err = r.cl.DeleteQueued(ctx, run.PromptID)
	case "running":
		err = r.cl.Interrupt(ctx, run.PromptID)
	default:
		entry, herr := r.cl.History(ctx, run.PromptID)
		if herr != nil {
			return upstream(herr)
		}
		if entry != nil && (entry.Status.Completed || entry.Status.StatusStr == "error") {
			return fmt.Errorf("%w: run %s has already finished in ComfyUI", store.ErrExists, run.ID)
		}
	}
	if err != nil {
		return upstream(err)
	}
	now := time.Now().UTC()
	run.Status = store.StatusCancelled
	run.Error = ""
	run.FinishedAt = &now
	if err := r.st.PutRun(*run); err != nil {
		return err
	}
	r.log.Info("run cancelled", "run", run.ID)
	return nil
}

func (r *Runner) follow(run store.Run, cancelCh chan cancelReq) {
	defer r.wg.Done()
	defer r.unregister(run.ID)
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	tick := time.NewTicker(r.poll)
	defer tick.Stop()

	failures, delay := 0, r.poll
	var firstFailure time.Time
	for {
		select {
		case <-ctx.Done():
			if r.ctx.Err() == nil {
				r.fail(&run, fmt.Sprintf("timed out after %s", r.timeout))
			}
			return
		case req := <-cancelCh:
			// Bounded by the caller's deadline, and by the run's own context
			// so a shutdown does not wait for a slow ComfyUI.
			cctx, ccancel := context.WithCancel(req.ctx)
			stop := context.AfterFunc(ctx, ccancel)
			err := r.doCancel(cctx, &run)
			stop()
			ccancel()
			req.reply <- err
			if err == nil {
				return
			}
			continue // could not cancel (ComfyUI down, already done): keep following
		case <-tick.C:
		}

		entry, err := r.cl.History(ctx, run.PromptID)
		if err != nil {
			if failures == 0 {
				firstFailure = time.Now()
			}
			failures++
			r.log.Warn("history poll failed", "run", run.ID, "err", err, "consecutive", failures)
			if time.Since(firstFailure) >= lostAfter {
				r.fail(&run, fmt.Sprintf("lost contact with ComfyUI for %s: %v", lostAfter, err))
				return
			}
			delay = max(r.poll, min(delay*2, maxBackoff))
			tick.Reset(delay)
			continue
		}
		if failures > 0 {
			failures, delay = 0, r.poll
			tick.Reset(r.poll)
		}

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
	n, err := io.Copy(tmp, io.LimitReader(rc, maxOutputBytes+1))
	if err == nil && n > maxOutputBytes {
		err = fmt.Errorf("output %s is larger than %d bytes", filepath.Base(dest), int64(maxOutputBytes))
	}
	if err != nil {
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
