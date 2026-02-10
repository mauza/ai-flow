package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/git"
	"github.com/mauza/ai-flow/internal/linear"
	"github.com/mauza/ai-flow/internal/orchestrator"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/subprocess"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	dbPath := flag.String("db", "ai-flow.db", "path to SQLite database")
	flag.Parse()

	// Structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("loading config", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"port", cfg.Server.Port,
		"team", cfg.Linear.TeamKey,
		"stages", len(cfg.Pipeline),
	)

	// Init store
	db, err := store.New(*dbPath)
	if err != nil {
		slog.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	slog.Info("database initialized", "path", *dbPath)

	// Init Linear client and load workflow states
	client := linear.NewClient(cfg.Linear.APIKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := client.LoadWorkflowStates(ctx, cfg.Linear.TeamKey); err != nil {
		cancel()
		slog.Error("loading workflow states from Linear", "error", err)
		os.Exit(1)
	}
	cancel()

	// Validate that all pipeline states exist in Linear
	for _, stage := range cfg.Pipeline {
		if _, ok := client.ResolveStateID(stage.LinearState); !ok {
			slog.Error("pipeline state not found in Linear",
				"stage", stage.Name,
				"linearState", stage.LinearState,
			)
			os.Exit(1)
		}
		if _, ok := client.ResolveStateID(stage.NextState); !ok {
			slog.Error("next state not found in Linear",
				"stage", stage.Name,
				"nextState", stage.NextState,
			)
			os.Exit(1)
		}
	}

	// Init git manager (optional — only when project is configured)
	var gitMgr *git.Manager
	if cfg.Project != nil {
		var err error
		gitMgr, err = git.NewManager()
		if err != nil {
			slog.Warn("git manager not available, PR creation disabled", "error", err)
		} else {
			slog.Info("git manager initialized", "repo", cfg.Project.GithubRepo)
		}
	}

	// Init runner and orchestrator
	runner := subprocess.NewRunner(cfg.Subprocess.MaxConcurrent)
	orch := orchestrator.New(cfg, client, db, runner, gitMgr)

	// Set up HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", linear.NewWebhookHandler(
		cfg.Linear.WebhookSecret,
		func(payload linear.WebhookPayload) {
			orch.HandleWebhook(context.Background(), payload)
		},
	))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("server starting", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
}
