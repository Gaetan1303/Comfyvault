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
