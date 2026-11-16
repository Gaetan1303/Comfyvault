package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Gaetan1303/comfyvault/internal/api"
	"github.com/Gaetan1303/comfyvault/internal/comfy"
	"github.com/Gaetan1303/comfyvault/internal/config"
	"github.com/Gaetan1303/comfyvault/internal/runner"
	"github.com/Gaetan1303/comfyvault/internal/store"
	"github.com/Gaetan1303/comfyvault/internal/workflow"
)

var version = "dev"

const usage = `comfyvault - store, template and serve ComfyUI workflows as an API

Usage:
  comfyvault [serve]                       start the HTTP server
  comfyvault import [flags] <workflow.json>
      -slug string       identifier of the workflow (required)
      -name string       display name
      -desc string       description
      -bindings string   JSON file with bindings; suggested automatically when omitted
  comfyvault suggest <workflow.json>       print suggested bindings as JSON
  comfyvault prune [flags]                 delete old finished runs and their images
      -older-than string  minimum age, e.g. 12h, 30d, 2w (required)
      -dry-run            only report what would be deleted
      -include-favorites  also delete runs marked as favorite
      -force              prune even though a service answers on COMFYVAULT_LISTEN
  comfyvault version

Configuration comes from the environment or a .env file; see .env.example.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "comfyvault:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve()
	case "import":
		return importCmd(args)
	case "suggest":
		return suggestCmd(args)
	case "prune":
		return pruneCmd(args)
	case "version":
		fmt.Println("comfyvault", version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	cl := comfy.New(cfg.ComfyURL)
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := cl.Ping(pingCtx); err != nil {
		log.Warn("ComfyUI is not reachable yet; runs will fail until it is", "url", cfg.ComfyURL, "err", err)
	}
	cancel()

	rn := runner.New(st, cl, cfg.PollInterval, cfg.RunTimeout, log)
	if err := rn.Recover(); err != nil {
		return fmt.Errorf("recovering runs: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(st, cl, rn, cfg.APIKey, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Bodies are capped in size; these cap how long they may take, so a
		// client trickling bytes cannot hold a connection forever. There is no
		// WriteTimeout on purpose: it would cut off large image downloads.
		ReadTimeout: 2 * time.Minute,
		IdleTimeout: 2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "comfyui", cfg.ComfyURL, "data", cfg.DataDir, "version", version)

	select {
	case err := <-errc:
		rn.Close()
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	rn.Close()
	return err
}

func importCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	slug := fs.String("slug", "", "identifier of the workflow")
	name := fs.String("name", "", "display name")
	desc := fs.String("desc", "", "description")
	bpath := fs.String("bindings", "", "JSON file with bindings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" || fs.NArg() != 1 {
		return errors.New("usage: comfyvault import -slug <slug> [-name ...] [-bindings file.json] <workflow.json>")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var bindings []workflow.Binding
	if *bpath != "" {
		braw, err := os.ReadFile(*bpath)
		if err != nil {
			return err
		}
		bindings = []workflow.Binding{}
		if err := json.Unmarshal(braw, &bindings); err != nil {
			return fmt.Errorf("%s: %w", *bpath, err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	_, v, created, err := st.PublishWorkflow(*slug, *name, *desc, raw, bindings)
	if err != nil {
		return err
	}
	if !created {
		fmt.Printf("%s: unchanged, still at version %d\n", *slug, v.Version)
	} else {
		fmt.Printf("%s: stored as version %d\n", *slug, v.Version)
	}
	fmt.Println("parameters:")
	seen := map[string]bool{}
	for _, b := range v.Bindings {
		if seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		req := ""
		if b.Required {
			req = " (required)"
		}
		fmt.Printf("  %-18s %-7s node %s.%s%s\n", b.Name, b.Type, b.NodeID, b.Input, req)
	}
	return nil
}

func suggestCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: comfyvault suggest <workflow.json>")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	g, err := workflow.Parse(raw)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(workflow.Suggest(g))
}

func pruneCmd(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	older := fs.String("older-than", "", "minimum age of the runs to delete (12h, 30d, 2w)")
	dry := fs.Bool("dry-run", false, "only report what would be deleted")
	favs := fs.Bool("include-favorites", false, "also delete runs marked as favorite")
	force := fs.Bool("force", false, "prune even though a service answers on COMFYVAULT_LISTEN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *older == "" || fs.NArg() != 0 {
		return errors.New("usage: comfyvault prune -older-than <age> [-dry-run] [-include-favorites]")
	}
	age, err := parseAge(*older)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !*dry && !*force && serviceAnswers(cfg.Listen) {
		// Another process writes runs under its own lock, which this one cannot
		// take. Through the service the same pruning is race-free.
		return fmt.Errorf("a comfyvault service answers on %s: stop it, use POST /api/v1/library/prune (or the Bibliothèque tab), or pass -force", cfg.Listen)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	removed, freed, err := st.Prune(time.Now().UTC().Add(-age), *favs, *dry)
	if err != nil {
		return err
	}
	verb := "deleted"
	if *dry {
		verb = "would delete"
	}
	fmt.Printf("%s %d run(s), %.1f MiB\n", verb, len(removed), float64(freed)/(1<<20))
	return nil
}

// parseAge accepts Go durations plus d (days) and w (weeks) suffixes.
func parseAge(s string) (time.Duration, error) {
	if len(s) > 1 {
		unit := map[byte]time.Duration{'d': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
		if u, ok := unit[s[len(s)-1]]; ok {
			n, err := strconv.Atoi(s[:len(s)-1])
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid age %q (try 30d, 2w or 12h)", s)
			}
			return time.Duration(n) * u, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (try 30d, 2w or 12h)", s)
	}
	return d, nil
}

// serviceAnswers reports whether something serves /healthz on listen.
func serviceAnswers(listen string) bool {
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get("http://" + host + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
