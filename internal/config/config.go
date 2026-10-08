// Package config loads the three kinds of ai-flow configuration: the
// Environment (where things run and which endpoints to use), the Catalog (the
// menu the planner picks from) and Projects (per-project settings).
//
// A config directory holds any number of *.yaml files; each YAML document
// declares its kind. Secrets are never inlined: fields ending in Env name the
// environment variable that holds the value.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mauza/ai-flow/internal/flow"
)

type Config struct {
	Env      Environment
	Catalog  Catalog
	Projects map[string]*Project
	Dir      string
}

type header struct {
	Kind string `json:"kind"`
}

// ---- Environment ----

type Environment struct {
	APIVersion  string      `json:"apiVersion,omitempty"`
	Kind        string      `json:"kind,omitempty"`
	Server      Server      `json:"server"`
	Runs        Runs        `json:"runs"`
	LLM         LLM         `json:"llm"`
	ObjectStore ObjectStore `json:"objectStore"`
	Git         Git         `json:"git"`
	GitHub      GitHub      `json:"github"`
	Linear      Linear      `json:"linear"`
	MCP         MCP         `json:"mcp"`
	Notify      Notify      `json:"notify"`
	Metrics     Metrics     `json:"metrics"`
}

// Metrics is a Prometheus-compatible query API (VictoriaMetrics, Prometheus)
// that check_health reads.
type Metrics struct {
	URL string `json:"url,omitempty"`
}

type Server struct {
	Listen    string `json:"listen,omitempty"`    // UI + API
	PodListen string `json:"podListen,omitempty"` // broker, git, llm, mcp — the only port runner pods reach
	PublicURL string `json:"publicUrl,omitempty"` // used in Linear comments
	PodURL    string `json:"podUrl,omitempty"`    // how runner pods reach PodListen
	DataDir   string `json:"dataDir,omitempty"`
	// Optional shared token for the UI/API. Empty = no auth (local kind).
	AuthTokenEnv string `json:"authTokenEnv,omitempty"`
}

type Runs struct {
	Namespace       string        `json:"namespace,omitempty"`
	MaxConcurrent   int           `json:"maxConcurrent,omitempty"`
	ServiceAccount  string        `json:"serviceAccount,omitempty"`
	TokenAudience   string        `json:"tokenAudience,omitempty"`
	ImagePullPolicy string        `json:"imagePullPolicy,omitempty"`
	DefaultTimeout  flow.Duration `json:"defaultTimeout,omitzero"`
	JobTTL          flow.Duration `json:"jobTTL,omitzero"`
	CPU             string        `json:"cpu,omitempty"`
	Memory          string        `json:"memory,omitempty"`
	// NodeSelector pins node pods, e.g. {kubernetes.io/arch: amd64} on a
	// mixed-arch cluster whose runtime images are single-arch.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Local mode runs pod-type nodes as child processes instead of Jobs (tests, no cluster).
	Local bool `json:"local,omitempty"`
}

type LLM struct {
	Upstreams map[string]Upstream `json:"upstreams"`
}

type Upstream struct {
	BaseURL   string `json:"baseUrl"`
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
}

type ObjectStore struct {
	Type string `json:"type,omitempty"` // s3 | garage | local
	// garage: the control plane bootstraps layout, key and bucket through the
	// admin API, so no S3 credentials need to be configured.
	AdminEndpoint string `json:"adminEndpoint,omitempty"`
	AdminTokenEnv string `json:"adminTokenEnv,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Region       string `json:"region,omitempty"`
	Secure       bool   `json:"secure,omitempty"`
	AccessKeyEnv string `json:"accessKeyEnv,omitempty"`
	SecretKeyEnv string `json:"secretKeyEnv,omitempty"`
}

// Git configures the upstream credentials the git proxy injects, per host.
type Git struct {
	Hosts map[string]GitHost `json:"hosts"`
	// AuthorName/Email for commits made by nodes.
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`
}

type GitHost struct {
	TokenEnv string `json:"tokenEnv"`
	Username string `json:"username,omitempty"` // default x-access-token
	Scheme   string `json:"scheme,omitempty"`   // default https
}

type GitHub struct {
	APIURL   string `json:"apiUrl,omitempty"`
	TokenEnv string `json:"tokenEnv,omitempty"`
}

type Linear struct {
	Enabled          bool          `json:"enabled"`
	Mode             string        `json:"mode,omitempty"` // poll | webhook
	PollInterval     flow.Duration `json:"pollInterval,omitzero"`
	APIKeyEnv        string        `json:"apiKeyEnv,omitempty"`
	WebhookSecretEnv string        `json:"webhookSecretEnv,omitempty"`
}

type MCP struct {
	Servers map[string]MCPServer `json:"servers"`
}

type MCPServer struct {
	URL string `json:"url"`
	// Header values may reference env vars: "Bearer ${GITHUB_TOKEN}".
	Headers     map[string]string `json:"headers,omitempty"`
	Description string            `json:"description,omitempty"`
}

// Notify pushes a message when a run needs a human. Off unless a backend is set.
type Notify struct {
	Ntfy *Ntfy `json:"ntfy,omitempty"`
	// Events to send: gate (a gate opened), stuck (a gate still waits after
	// stuckAfter), failed, succeeded. Default: gate, stuck, failed.
	Events     []string      `json:"events,omitempty"`
	StuckAfter flow.Duration `json:"stuckAfter,omitzero"` // default 4h
}

type Ntfy struct {
	URL      string `json:"url"`   // server, e.g. https://ntfy.sh
	Topic    string `json:"topic"` // a topic the token may publish to
	TokenEnv string `json:"tokenEnv,omitempty"`
}

// Notification events.
const (
	NotifyGate      = "gate"
	NotifyStuck     = "stuck"
	NotifyFailed    = "failed"
	NotifySucceeded = "succeeded"
)

// Sends reports whether event is configured to notify.
func (n Notify) Sends(event string) bool {
	return n.Ntfy != nil && slices.Contains(n.Events, event)
}

// ---- Catalog ----

type Catalog struct {
	APIVersion string             `json:"apiVersion,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Models     map[string]*Model  `json:"models"`
	Harnesses  map[string]Harness `json:"harnesses,omitempty"`
	Runtimes   map[string]Runtime `json:"runtimes"`
	Grants     map[string]*Grant  `json:"grants,omitempty"`
	Skills     map[string]*Skill  `json:"skills,omitempty"`
	Presets    map[string]*Preset `json:"presets,omitempty"`
	Actions    []string           `json:"-"`
	Planner    Planner            `json:"planner"`
	// Node defaults applied to every flow (runtime, harness, timeout, retry, llm).
	Defaults *flow.Node `json:"defaults,omitempty"`
}

type Model struct {
	Upstream       string          `json:"upstream"`
	Model          string          `json:"model"`
	Size           string          `json:"size,omitempty"` // small | medium | large | frontier
	Ctx            string          `json:"ctx,omitempty"`
	ContextTokens  int             `json:"context_tokens,omitempty"`
	ToolUse        string          `json:"tool_use,omitempty"`
	Cost           string          `json:"cost,omitempty"`
	InputPer1M     float64         `json:"input_per_1m,omitempty"`
	OutputPer1M    float64         `json:"output_per_1m,omitempty"`
	MaxConcurrency int             `json:"max_concurrency,omitempty"`
	Reasoning      bool            `json:"reasoning,omitempty"`
	Notes          string          `json:"notes,omitempty"`
	LLM            *flow.LLMConfig `json:"llm,omitempty"`
}

type Harness struct {
	Description string `json:"description,omitempty"`
}

type Runtime struct {
	Image       string `json:"image"`
	Description string `json:"description,omitempty"`
}

const (
	GrantGit    = "git"
	GrantMCP    = "mcp"
	GrantSecret = "secret"
	GrantEgress = "egress"
)

type Grant struct {
	Kind        string     `json:"kind"`
	Description string     `json:"description,omitempty"`
	URL         string     `json:"url,omitempty"`   // git
	Modes       []string   `json:"modes,omitempty"` // git: read, write
	Server      string     `json:"server,omitempty"`
	Tools       []string   `json:"tools,omitempty"` // mcp
	SecretRef   *SecretRef `json:"secretRef,omitempty"`
	Env         string     `json:"env,omitempty"`   // secret → env var
	File        string     `json:"file,omitempty"`  // secret → file path in the pod
	Allow       string     `json:"allow,omitempty"` // egress: internet
}

type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type Skill struct {
	Description string            `json:"description,omitempty"`
	Path        string            `json:"path,omitempty"`  // dir relative to the config dir
	Files       map[string]string `json:"files,omitempty"` // inline: relative path → content
}

type Preset struct {
	flow.Node
	// Discovery metadata only; these hints do not grant capabilities or alter execution.
	Category   string   `json:"category,omitempty"`
	WhenToUse  string   `json:"when_to_use,omitempty"`
	Requires   []string `json:"requires,omitempty"`
	MinSize    string   `json:"min_size,omitempty"`
	PromptFile string   `json:"prompt_file,omitempty"`
}

type Planner struct {
	Model    string `json:"model"`
	Guidance string `json:"guidance,omitempty"`
	Stream   bool   `json:"stream,omitempty"`
	// Max planner attempts when the draft fails validation.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// ---- Project ----

type Project struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Metadata   ProjectMetadata `json:"metadata"`
	Spec       ProjectSpec     `json:"spec"`
}

type ProjectMetadata struct {
	Name string `json:"name"`
}

type ProjectSpec struct {
	Description string       `json:"description,omitempty"`
	Linear      *LinearLink  `json:"linear,omitempty"`
	Repo        string       `json:"repo"` // git grant name
	Base        string       `json:"base,omitempty"`
	Start       string       `json:"start,omitempty"` // manual | auto
	Allow       Allow        `json:"allow"`
	Budget      *ProjBudget  `json:"budget,omitempty"`
	Planner     PlannerGuide `json:"planner,omitempty"`
	Defaults    *flow.Node   `json:"defaults,omitempty"`
	Deploy      *Deploy      `json:"deploy,omitempty"`
}

// Deploy says that merging a PR into Branch ships the project to production
// (its CI/CD builds and rolls out main). wait_for_deploy and check_health read
// the rest; the validator then requires CI before a merge and monitoring after.
type Deploy struct {
	Branch   string         `json:"branch,omitempty"`   // default: the project base
	URL      string         `json:"url,omitempty"`      // check_health probes it (expects 2xx)
	Versions []VersionProbe `json:"versions,omitempty"` // wait_for_deploy: all must report the merged commit
	Health   []HealthQuery  `json:"health,omitempty"`   // check_health: PromQL, value above max is a violation
}

// VersionProbe is an endpoint reporting the deployed commit: the whole body, or
// a top-level JSON field.
type VersionProbe struct {
	URL   string `json:"url"`
	Field string `json:"field,omitempty"`
}

// HealthQuery is a PromQL instant query; the highest series value must stay at
// or below Max. An empty result counts as 0, so write queries where no data is
// healthy (e.g. a 5xx rate).
type HealthQuery struct {
	Name  string  `json:"name"`
	Query string  `json:"query"`
	Max   float64 `json:"max"`
}

type LinearLink struct {
	Team     string   `json:"team"`
	Projects []string `json:"projects,omitempty"`
	Trigger  struct {
		Label  string   `json:"label"`
		States []string `json:"states"`
	} `json:"trigger"`
	States struct {
		Planning  string `json:"planning,omitempty"`
		FlowReady string `json:"flow_ready,omitempty"`
		Running   string `json:"running,omitempty"`
		Succeeded string `json:"succeeded,omitempty"`
		Failed    string `json:"failed,omitempty"`
	} `json:"states"`
	Comments struct {
		Flow   bool `json:"flow"`
		Result bool `json:"result"`
	} `json:"comments"`
}

type Allow struct {
	Grants []string `json:"grants,omitempty"` // patterns: "repo/x:*", "mcp/*"
	Models []string `json:"models,omitempty"`
}

type ProjBudget struct {
	USDPerRun   float64 `json:"usd_per_run,omitempty"`
	USDPerMonth float64 `json:"usd_per_month,omitempty"`
}

type PlannerGuide struct {
	Guidance string `json:"guidance,omitempty"`
}

// Load reads config from directories (every *.yaml / *.yml, non-recursive) and
// files, in order. A later Environment or Catalog document replaces an
// earlier one, so a local override file can follow the shared directory.
// Relative skill paths and prompt files resolve against the catalog's directory.
func Load(paths ...string) (*Config, error) {
	cfg := &Config{Projects: map[string]*Project{}}
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		var dirFiles []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if ext := filepath.Ext(e.Name()); ext == ".yaml" || ext == ".yml" {
				dirFiles = append(dirFiles, filepath.Join(p, e.Name()))
			}
		}
		sort.Strings(dirFiles)
		files = append(files, dirFiles...)
	}
	var haveEnv, haveCatalog bool
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for i, doc := range splitDocs(data) {
			var h header
			if err := yaml.Unmarshal(doc, &h); err != nil {
				return nil, fmt.Errorf("%s doc %d: %w", f, i+1, err)
			}
			where := fmt.Sprintf("%s doc %d (%s)", filepath.Base(f), i+1, h.Kind)
			switch h.Kind {
			case "Environment":
				cfg.Env = Environment{}
				if err := yaml.UnmarshalStrict(doc, &cfg.Env); err != nil {
					return nil, fmt.Errorf("%s: %w", where, err)
				}
				haveEnv = true
			case "Catalog":
				cfg.Catalog = Catalog{}
				if err := yaml.UnmarshalStrict(doc, &cfg.Catalog); err != nil {
					return nil, fmt.Errorf("%s: %w", where, err)
				}
				cfg.Dir = filepath.Dir(f)
				haveCatalog = true
			case "Project":
				var p Project
				if err := yaml.UnmarshalStrict(doc, &p); err != nil {
					return nil, fmt.Errorf("%s: %w", where, err)
				}
				if p.Metadata.Name == "" {
					return nil, fmt.Errorf("%s: metadata.name is required", where)
				}
				cfg.Projects[p.Metadata.Name] = &p
			case "":
				continue
			default:
				return nil, fmt.Errorf("%s: unknown kind %q", where, h.Kind)
			}
		}
	}
	if !haveCatalog {
		return nil, fmt.Errorf("no Catalog document found in %s", strings.Join(paths, ", "))
	}
	if !haveEnv {
		cfg.Env = Environment{}
	}
	cfg.applyDefaults()
	if err := cfg.loadFiles(); err != nil {
		return nil, err
	}
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func splitDocs(data []byte) [][]byte {
	var out [][]byte
	for _, part := range bytes.Split(append([]byte("\n"), data...), []byte("\n---")) {
		if len(bytes.TrimSpace(part)) > 0 {
			out = append(out, part)
		}
	}
	return out
}

func (c *Config) applyDefaults() {
	e := &c.Env
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&e.Server.Listen, ":8080")
	def(&e.Server.PodListen, ":8081")
	def(&e.Server.PublicURL, "http://localhost:8080")
	def(&e.Server.PodURL, "http://ai-flow-pods.ai-flow.svc:8081")
	def(&e.Server.DataDir, "data")
	def(&e.Runs.Namespace, "ai-flow-runs")
	def(&e.Runs.ServiceAccount, "ai-flow-runner")
	def(&e.Runs.TokenAudience, "ai-flow")
	def(&e.Runs.ImagePullPolicy, "IfNotPresent")
	def(&e.Runs.CPU, "2")
	def(&e.Runs.Memory, "4Gi")
	if e.Runs.MaxConcurrent == 0 {
		e.Runs.MaxConcurrent = 3
	}
	if e.Notify.Ntfy != nil && e.Notify.Events == nil {
		e.Notify.Events = []string{NotifyGate, NotifyStuck, NotifyFailed}
	}
	if e.Notify.StuckAfter.Duration == 0 {
		e.Notify.StuckAfter.Duration = 4 * time.Hour
	}
	if e.Runs.DefaultTimeout.Duration == 0 {
		e.Runs.DefaultTimeout.Duration = 30 * time.Minute
	}
	if e.Runs.JobTTL.Duration == 0 {
		e.Runs.JobTTL.Duration = 24 * time.Hour
	}
	def(&e.ObjectStore.Type, "local")
	def(&e.ObjectStore.Bucket, "ai-flow")
	def(&e.ObjectStore.Region, "garage")
	def(&e.ObjectStore.AccessKeyEnv, "S3_ACCESS_KEY_ID")
	def(&e.ObjectStore.SecretKeyEnv, "S3_SECRET_ACCESS_KEY")
	def(&e.Git.AuthorName, "ai-flow")
	def(&e.Git.AuthorEmail, "ai-flow@localhost")
	if e.Git.Hosts == nil {
		e.Git.Hosts = map[string]GitHost{"github.com": {TokenEnv: "GITHUB_TOKEN"}}
	}
	def(&e.GitHub.APIURL, "https://api.github.com")
	def(&e.GitHub.TokenEnv, "GITHUB_TOKEN")
	def(&e.Linear.Mode, "poll")
	def(&e.Linear.APIKeyEnv, "LINEAR_API_KEY")
	def(&e.Linear.WebhookSecretEnv, "LINEAR_WEBHOOK_SECRET")
	if e.Linear.PollInterval.Duration == 0 {
		e.Linear.PollInterval.Duration = 30 * time.Second
	}

	cat := &c.Catalog
	if cat.Harnesses == nil {
		cat.Harnesses = map[string]Harness{"pi": {Description: "pi coding agent"}}
	}
	for _, m := range [](*map[string]*Grant){&cat.Grants} {
		if *m == nil {
			*m = map[string]*Grant{}
		}
	}
	if cat.Skills == nil {
		cat.Skills = map[string]*Skill{}
	}
	if cat.Presets == nil {
		cat.Presets = map[string]*Preset{}
	}
	if cat.Planner.MaxAttempts == 0 {
		cat.Planner.MaxAttempts = 3
	}
	cat.Actions = flow.ActionNames()
	for _, p := range c.Projects {
		def(&p.Spec.Base, "main")
		def(&p.Spec.Start, "manual")
		if p.Spec.Deploy != nil {
			def(&p.Spec.Deploy.Branch, p.Spec.Base)
		}
	}
}

// loadFiles reads skill directories and preset prompt files into memory.
func (c *Config) loadFiles() error {
	for name, s := range c.Catalog.Skills {
		if s.Path == "" {
			continue
		}
		root := filepath.Join(c.Dir, s.Path)
		if s.Files == nil {
			s.Files = map[string]string{}
		}
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			s.Files[rel] = string(b)
			return nil
		})
		if err != nil {
			return fmt.Errorf("skill %s: %w", name, err)
		}
	}
	for name, p := range c.Catalog.Presets {
		if p.PromptFile == "" || p.Prompt != "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.Dir, p.PromptFile))
		if err != nil {
			return fmt.Errorf("preset %s: %w", name, err)
		}
		p.Prompt = string(b)
	}
	return nil
}

func (c *Config) check() error {
	var errs []string
	for name, m := range c.Catalog.Models {
		if _, ok := c.Env.LLM.Upstreams[m.Upstream]; !ok {
			errs = append(errs, fmt.Sprintf("model %s: unknown upstream %q", name, m.Upstream))
		}
		if m.Model == "" {
			errs = append(errs, fmt.Sprintf("model %s: model is required", name))
		}
	}
	if c.Catalog.Planner.Model != "" {
		if _, ok := c.Catalog.Models[c.Catalog.Planner.Model]; !ok {
			errs = append(errs, fmt.Sprintf("planner.model: unknown model %q", c.Catalog.Planner.Model))
		}
	}
	for name, g := range c.Catalog.Grants {
		switch g.Kind {
		case GrantGit:
			if g.URL == "" {
				errs = append(errs, fmt.Sprintf("grant %s: url is required", name))
			}
		case GrantMCP:
			if _, ok := c.Env.MCP.Servers[g.Server]; !ok {
				errs = append(errs, fmt.Sprintf("grant %s: unknown mcp server %q", name, g.Server))
			}
		case GrantSecret:
			if g.SecretRef == nil || (g.Env == "" && g.File == "") {
				errs = append(errs, fmt.Sprintf("grant %s: secretRef and env are required", name))
			}
		case GrantEgress:
		default:
			errs = append(errs, fmt.Sprintf("grant %s: unknown kind %q", name, g.Kind))
		}
	}
	for name, p := range c.Projects {
		if g, ok := c.Catalog.Grants[p.Spec.Repo]; !ok || g.Kind != GrantGit {
			errs = append(errs, fmt.Sprintf("project %s: repo %q is not a git grant", name, p.Spec.Repo))
		}
		if p.Spec.Start != "manual" && p.Spec.Start != "auto" {
			errs = append(errs, fmt.Sprintf("project %s: start must be manual or auto", name))
		}
		if d := p.Spec.Deploy; d != nil {
			if len(d.Health) > 0 && c.Env.Metrics.URL == "" {
				errs = append(errs, fmt.Sprintf("project %s: deploy.health needs metrics.url in the environment", name))
			}
			for i, h := range d.Health {
				if h.Name == "" || h.Query == "" {
					errs = append(errs, fmt.Sprintf("project %s: deploy.health[%d] needs name and query", name, i))
				}
			}
			for i, v := range d.Versions {
				if v.URL == "" {
					errs = append(errs, fmt.Sprintf("project %s: deploy.versions[%d] needs url", name, i))
				}
			}
		}
	}
	if n := c.Env.Notify.Ntfy; n != nil && (n.URL == "" || n.Topic == "") {
		errs = append(errs, "notify.ntfy: url and topic are required")
	}
	for _, ev := range c.Env.Notify.Events {
		if !slices.Contains([]string{NotifyGate, NotifyStuck, NotifyFailed, NotifySucceeded}, ev) {
			errs = append(errs, fmt.Sprintf("notify.events: unknown event %q (gate, stuck, failed, succeeded)", ev))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Secret reads the value of an env var named by a *Env field.
func Secret(envName string) string {
	if envName == "" {
		return ""
	}
	return os.Getenv(envName)
}

// AllowsGrant reports whether the project may use grant (with optional :mode).
func (p *Project) AllowsGrant(grant string) bool {
	if p == nil {
		return true
	}
	name, mode, _ := strings.Cut(grant, ":")
	for _, pat := range p.Spec.Allow.Grants {
		pn, pm, hasMode := strings.Cut(pat, ":")
		if ok, _ := filepath.Match(pn, name); !ok {
			continue
		}
		if !hasMode || pm == "*" || mode == "" || pm == mode {
			return true
		}
	}
	return false
}

func (p *Project) AllowsModel(model string) bool {
	if p == nil || len(p.Spec.Allow.Models) == 0 {
		return true
	}
	for _, pat := range p.Spec.Allow.Models {
		if ok, _ := filepath.Match(pat, model); ok {
			return true
		}
	}
	return false
}
