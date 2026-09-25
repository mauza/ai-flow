// Command ai-flow: plan tasks into flows and run them as Kubernetes Jobs.
//
//	ai-flow server   -config deploy/config [-config local.yaml] [-local]
//	ai-flow node                         (entrypoint of every node pod)
//	ai-flow validate -config deploy/config flow.yaml
//	ai-flow plan     -config deploy/config -project sandbox -title "..." [-body "..."]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/broker"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/intake"
	"github.com/mauza/ai-flow/internal/launcher"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/mcpdemo"
	"github.com/mauza/ai-flow/internal/mcpx"
	"github.com/mauza/ai-flow/internal/objstore"
	"github.com/mauza/ai-flow/internal/planner"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/runner"
	"github.com/mauza/ai-flow/internal/server"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/web"
)

var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "server":
		err = serverCmd(ctx, os.Args[2:])
	case "node":
		err = runner.Main(ctx)
	case "validate":
		err = validateCmd(os.Args[2:])
	case "plan":
		err = planCmd(ctx, os.Args[2:])
	case "mcp-demo":
		listen := ":8090"
		if len(os.Args) > 2 {
			listen = os.Args[2]
		}
		slog.Info("demo MCP server", "listen", listen, "tools", "word_stats, time_now")
		err = http.ListenAndServe(listen, mcpdemo.Handler())
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ai-flow server|node|validate|plan|mcp-demo|version [flags]")
}

func logLevel() slog.Level {
	if os.Getenv("AI_FLOW_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	var paths multiFlag
	fs.Var(&paths, "config", "config directory or file (repeatable; later files override)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		if env := os.Getenv("AI_FLOW_CONFIG"); env != "" {
			paths = strings.Split(env, ",")
		} else {
			paths = []string{"deploy/config"}
		}
	}
	return config.Load(paths...)
}

func serverCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	local := fs.Bool("local", false, "run nodes as local processes instead of Kubernetes Jobs")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if *local {
		cfg.Env.Runs.Local = true
	}
	env := &cfg.Env
	if err := os.MkdirAll(env.Server.DataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(env.Server.DataDir, "ai-flow.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	signer, err := grant.LoadOrCreate(env.Server.DataDir)
	if err != nil {
		return err
	}
	obj, err := objstore.New(ctx, env.ObjectStore, env.Server.DataDir)
	if err != nil {
		return err
	}
	h := hub.New()
	gh := github.New(env.GitHub.APIURL, config.Secret(env.GitHub.TokenEnv))

	var launch engine.Launcher
	var pods broker.PodIdentifier
	if env.Runs.Local {
		podURL := "http://127.0.0.1" + portOf(env.Server.PodListen)
		env.Server.PodURL = podURL
		launch = launcher.NewLocal(podURL, env.Server.DataDir, signer)
		slog.Info("local mode: nodes run as child processes (no isolation)")
	} else {
		cs, err := launcher.Client()
		if err != nil {
			return err
		}
		k := launcher.NewKube(cs, env)
		launch, pods = k, k
	}

	eng := engine.New(cfg, st, launch, h, gh)
	a := &app.App{Cfg: cfg, Store: st, Engine: eng, Planner: planner.New(cfg, llm.New(cfg), gh), Hub: h}
	br := broker.New(cfg, st, eng, signer, obj, h, pods, mcpx.NewPool(env.MCP.Servers))

	ui := server.New(a, obj, web.FS())
	var lin *intake.Linear
	if env.Linear.Enabled {
		if lin, err = intake.NewLinear(cfg, a); err != nil {
			slog.Warn("Linear intake disabled", "err", err)
			env.Linear.Enabled = false
			lin = nil
		}
	}
	if lin != nil {
		eng.SetHooks(lin)
		a.AddNotifier(lin)
		if env.Linear.Mode == "webhook" {
			h, err := lin.WebhookHandler()
			if err != nil {
				return err
			}
			ui.Mount("POST /webhooks/linear", h)
		}
		go lin.Run(ctx)
	}

	apiSrv := &http.Server{Addr: env.Server.Listen, Handler: logRequests(ui.Handler())}
	podSrv := &http.Server{Addr: env.Server.PodListen, Handler: br.Handler()}
	errc := make(chan error, 2)
	go func() { errc <- apiSrv.ListenAndServe() }()
	go func() { errc <- podSrv.ListenAndServe() }()
	go eng.Loop(ctx)
	slog.Info("ai-flow started", "version", version, "ui", env.Server.Listen, "pods", env.Server.PodListen, "runs", ternary(env.Runs.Local, "local", env.Runs.Namespace))

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apiSrv.Shutdown(shutdown)
	podSrv.Shutdown(shutdown)
	return nil
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ":" + listen
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/events" {
			slog.Debug("api", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}

func validateCmd(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: ai-flow validate -config dir flow.yaml...")
	}
	bad := false
	for _, path := range fs.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := flow.Parse(data)
		if err != nil {
			fmt.Printf("%s: %v\n", path, err)
			bad = true
			continue
		}
		issues := resolve.Validate(resolve.Resolve(f, cfg), cfg)
		if len(issues) == 0 {
			fmt.Printf("%s: ok\n", path)
		}
		for _, i := range issues {
			fmt.Printf("%s: %s\n", path, i)
		}
		bad = bad || resolve.HasErrors(issues)
	}
	if bad {
		return fmt.Errorf("validation failed")
	}
	return nil
}

func planCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	title := fs.String("title", "", "task title")
	body := fs.String("body", "", "task body")
	name := fs.String("name", "cli-plan", "flow name")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	gh := github.New(cfg.Env.GitHub.APIURL, config.Secret(cfg.Env.GitHub.TokenEnv))
	p := planner.New(cfg, llm.New(cfg), gh)
	start := time.Now()
	res, err := p.Plan(ctx, planner.Request{FlowName: *name, Project: *project, Title: *title, Body: *body})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "# attempts=%d valid=%v took=%s\n# %s\n", res.Attempts, res.Valid, time.Since(start).Round(time.Second), res.Explanation)
	for _, i := range res.Issues {
		fmt.Fprintf(os.Stderr, "# %s\n", i)
	}
	fmt.Println(res.YAML)
	return nil
}
