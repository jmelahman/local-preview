// Package orchestrator is the embeddable API of local-preview: everything
// the standalone server does — mirror clones, content-addressed builds,
// on-demand supervised backends, lineage-forked state, Host-header preview
// routing — behind a small facade another Go application can wire into its
// own HTTP server.
//
// Typical embedding:
//
//	orch, err := orchestrator.New(orchestrator.Options{
//		DataDir: filepath.Join(dataDir, "previews"),
//		Addr:    ":8080", // the host application's listen address
//	})
//	...
//	srv.Handler = orch.WrapHost(appHandler) // previews by subdomain, app otherwise
//	...
//	orch.RegisterRepo(ctx, "myapp", "/path/to/repo")
//	orch.RequestDeploy(ctx, "myapp", "some-branch", false)
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/proxy"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

// ErrNotFound is returned when a repo or deploy does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a name is already registered with a
// different source.
var ErrConflict = errors.New("conflict")

// RunSpec describes one build step handed to a Runner. ScratchDir is the
// extracted commit tree on the host; Dir is the working directory relative
// to it. Image is the manifest-declared build image for the step's side
// ("" = unspecified) — custom runners should honor it when set, since it's
// the repo's explicit contract.
type RunSpec struct {
	RepoName   string
	SHA        string
	ScratchDir string
	Dir        string
	Argv       []string
	Image      string
}

// Runner executes build steps. Omit it for host execution; inject an
// implementation to run steps inside a container (e.g. the target repo's
// devcontainer) for reproducible builds.
type Runner interface {
	Run(ctx context.Context, spec RunSpec, output io.Writer) error
}

// Options configures an Orchestrator.
type Options struct {
	// DataDir is the root for the orchestrator's database, mirror clones,
	// artifacts, state dirs, and logs. Required.
	DataDir string
	// Addr is the address the embedding server listens on, used only to
	// construct preview URLs (e.g. ":8080"). Optional.
	Addr string
	// PreviewDomain is the base domain previews are served under. Defaults
	// to "preview.localhost".
	PreviewDomain string
	// DBPath overrides the SQLite location; ":memory:" is allowed (deploy
	// history becomes ephemeral; artifacts still use DataDir).
	DBPath string
	// BuildConcurrency is the number of deploys built in parallel.
	// Defaults to 2.
	BuildConcurrency int
	// Runner executes build steps. Defaults to host execution.
	Runner Runner
	// MaxWarm caps concurrently running preview processes; beyond it the
	// least-recently-used are stopped. 0 defaults to 8; negative disables
	// the cap.
	MaxWarm int
	// ManifestSources are the locations tried, in order, for the preview
	// manifest at each deployed commit. Defaults to preview.toml at the
	// repo root. Embedders can add their own config file with a table —
	// e.g. {Path: ".kanban.toml", Table: "previews"}. Artifact hashes cover
	// the parsed manifest, not its location, so moving unchanged config
	// between sources doesn't invalidate caches.
	ManifestSources []ManifestSource
	// LocalManifestDir, when set, is a directory searched for out-of-repo
	// manifests (`<dir>/<repo>.toml`, plain preview.toml schema) when no
	// in-repo source matches — onboarding repos whose upstream can't carry a
	// manifest. The file is read from the server's disk at build time, not
	// from the deployed commit.
	LocalManifestDir string
}

// ManifestSource locates a preview manifest: a TOML file at the target
// repo's root, optionally rooted at a top-level table of that file.
type ManifestSource struct {
	Path  string
	Table string
}

// Repo is a registered repository.
type Repo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

// Deploy is one commit's preview deployment.
type Deploy struct {
	ID           int64  `json:"id"`
	Repo         string `json:"repo"`
	SHA          string `json:"sha"`
	ShortSHA     string `json:"short_sha"`
	Ref          string `json:"ref,omitempty"`
	Branch       string `json:"branch,omitempty"`
	AuthorName   string `json:"author_name,omitempty"`
	AuthorEmail  string `json:"author_email,omitempty"`
	FeHash       string `json:"fe_hash,omitempty"`
	BeHash       string `json:"be_hash,omitempty"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	AttemptCount int64  `json:"attempt_count"`
	// PreviewURL is set once the deploy is ready.
	PreviewURL string `json:"preview_url,omitempty"`
	// Process is the live backend state when ready: running, starting, or
	// stopped (backends start on demand). FeProcess is the same for a
	// process-mode frontend, absent for static frontends.
	Process   string `json:"process,omitempty"`
	FeProcess string `json:"fe_process,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Deploy statuses.
const (
	StatusQueued   = db.DeployQueued
	StatusBuilding = db.DeployBuilding
	StatusReady    = db.DeployReady
	StatusFailed   = db.DeployFailed
	StatusEvicted  = db.DeployEvicted
)

// Orchestrator owns the preview pipeline. Create with New; release with
// Close.
type Orchestrator struct {
	opts     Options
	database *db.Store
	files    *store.Store
	git      *gitrepo.Manager
	super    *supervise.Manager
	queue    *build.Queue
	stop     context.CancelFunc
}

// runnerAdapter bridges the public Runner onto the internal interface.
type runnerAdapter struct{ r Runner }

func (a runnerAdapter) Run(ctx context.Context, spec build.RunSpec, output io.Writer) error {
	return a.r.Run(ctx, RunSpec(spec), output)
}

// New starts an orchestrator: it verifies git, opens the database, sweeps
// stale scratch space, reclaims orphaned processes from an unclean exit,
// and launches the build workers.
func New(opts Options) (*Orchestrator, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("orchestrator: DataDir is required")
	}
	if opts.PreviewDomain == "" {
		opts.PreviewDomain = "preview.localhost"
	}
	if opts.BuildConcurrency <= 0 {
		opts.BuildConcurrency = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := gitrepo.CheckGit(ctx); err != nil {
		cancel()
		return nil, err
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		cancel()
		return nil, fmt.Errorf("orchestrator: create data dir: %w", err)
	}
	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = opts.DataDir + "/preview.db"
	}
	database, err := db.Open(dbPath)
	if err != nil {
		cancel()
		return nil, err
	}

	files := store.New(opts.DataDir+"/artifacts", opts.DataDir+"/state", opts.DataDir+"/tmp")
	files.SweepTmp(24 * time.Hour)
	gitMgr := gitrepo.NewManager(opts.DataDir + "/repos")
	if opts.MaxWarm == 0 {
		opts.MaxWarm = 8
	}
	super := supervise.New(database, files, opts.DataDir+"/logs")
	super.ReclaimOrphans()
	super.SetMaxWarm(opts.MaxWarm)
	super.StartReaper(ctx)

	var runner build.Runner
	if opts.Runner != nil {
		runner = runnerAdapter{r: opts.Runner}
	}
	queue := build.NewQueue(database, gitMgr, files, super, opts.DataDir+"/logs", runner)
	if len(opts.ManifestSources) > 0 {
		refs := make([]build.ManifestRef, len(opts.ManifestSources))
		for i, s := range opts.ManifestSources {
			refs[i] = build.ManifestRef{Path: s.Path, Table: s.Table}
		}
		queue.SetManifestRefs(refs)
	}
	if opts.LocalManifestDir != "" {
		queue.SetLocalManifestDir(opts.LocalManifestDir)
	}
	queue.Start(ctx, opts.BuildConcurrency)

	return &Orchestrator{
		opts:     opts,
		database: database,
		files:    files,
		git:      gitMgr,
		super:    super,
		queue:    queue,
		stop:     cancel,
	}, nil
}

// Close stops the build workers and every supervised backend process, then
// closes the database. Backends never outlive the orchestrator.
func (o *Orchestrator) Close() error {
	o.stop()
	o.super.StopAll()
	return o.database.Close()
}

// WrapHost returns the top-level handler: requests whose Host is a preview
// subdomain (<sha>.<repo>.<domain>) are served by the orchestrator;
// everything else falls through to next.
func (o *Orchestrator) WrapHost(next http.Handler) http.Handler {
	return proxy.New(o.database, o.files, o.super, o.opts.PreviewDomain, next)
}

// RegisterRepo registers a repository (mirror clone of source, a local path
// or clone URL). Idempotent: re-registering the same name+source returns
// the existing repo; the same name with a different source is ErrConflict.
// The name must be a lowercase DNS label — it becomes the subdomain segment.
func (o *Orchestrator) RegisterRepo(ctx context.Context, name, source string) (Repo, error) {
	if err := gitrepo.ValidateName(name); err != nil {
		return Repo{}, err
	}
	if existing, err := o.database.GetRepoByName(name); err == nil {
		if existing.Source == source {
			return toRepo(existing), nil
		}
		return Repo{}, fmt.Errorf("repo %q is registered with source %q: %w", name, existing.Source, ErrConflict)
	}
	gr, err := o.git.Add(ctx, name, source)
	if err != nil {
		return Repo{}, err
	}
	created, err := o.database.CreateRepo(name, source, gr.Path)
	if errors.Is(err, db.ErrConflict) {
		return Repo{}, ErrConflict
	}
	if err != nil {
		return Repo{}, err
	}
	return toRepo(created), nil
}

// DeleteRepo unregisters a repository: it stops the repo's supervised
// backends, removes its database rows, then deletes its mirror clone,
// artifacts, state directories, and build logs. On-disk cleanup failures
// after the rows are gone are ignored — leftovers are unreachable and only
// cost disk. Returns ErrNotFound if the name isn't registered.
func (o *Orchestrator) DeleteRepo(name string) error {
	repo, err := o.database.GetRepoByName(name)
	if errors.Is(err, db.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	o.super.StopRepo(repo.ID, "repo deleted")
	o.super.PurgeRepoContainers(repo.Name)
	if err := o.database.DeleteRepo(repo.ID); err != nil {
		return err
	}
	o.git.Remove(repo.Name)
	o.files.RemoveRepo(repo.Name)
	os.RemoveAll(o.opts.DataDir + "/logs/" + repo.Name)
	return nil
}

// Repos lists registered repositories.
func (o *Orchestrator) Repos() ([]Repo, error) {
	rows, err := o.database.ListRepos()
	if err != nil {
		return nil, err
	}
	out := make([]Repo, len(rows))
	for i, r := range rows {
		out[i] = toRepo(r)
	}
	return out, nil
}

// RequestDeploy resolves ref (branch, tag, or sha) in the named repo and
// queues a build if needed. Idempotent per commit; rebuild bypasses the
// artifact cache.
func (o *Orchestrator) RequestDeploy(ctx context.Context, repo, ref string, rebuild bool) (Deploy, error) {
	row, err := o.queue.RequestDeploy(ctx, repo, ref, rebuild)
	if errors.Is(err, db.ErrNotFound) {
		return Deploy{}, fmt.Errorf("repo %q: %w", repo, ErrNotFound)
	}
	if err != nil {
		return Deploy{}, err
	}
	return o.toDeploy(row), nil
}

// Deploy returns one deploy by ID.
func (o *Orchestrator) Deploy(id int64) (Deploy, error) {
	row, err := o.database.GetDeployByID(id)
	if errors.Is(err, db.ErrNotFound) {
		return Deploy{}, ErrNotFound
	}
	if err != nil {
		return Deploy{}, err
	}
	return o.toDeploy(row), nil
}

// Deploys lists deploys newest first, optionally filtered by repo name.
func (o *Orchestrator) Deploys(repo string) ([]Deploy, error) {
	rows, err := o.database.ListDeploys(db.DeployFilter{Repo: repo})
	if err != nil {
		return nil, err
	}
	out := make([]Deploy, len(rows))
	for i, row := range rows {
		out[i] = o.toDeploy(row)
	}
	return out, nil
}

// DeployLogs returns a plain-text snapshot of a deploy's build logs.
func (o *Orchestrator) DeployLogs(id int64) (string, error) {
	row, err := o.database.GetDeployByID(id)
	if errors.Is(err, db.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, part := range []struct{ title, path string }{
		{"frontend build", row.FeBuildLogPath},
		{"backend build", row.BeBuildLogPath},
	} {
		fmt.Fprintf(&b, "--- %s ---\n", part.title)
		if part.path == "" {
			b.WriteString("(not started)\n")
			continue
		}
		content, err := os.ReadFile(part.path)
		if err != nil {
			b.WriteString("(no log)\n")
			continue
		}
		b.Write(content)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func toRepo(r db.Repo) Repo {
	return Repo{ID: r.ID, Name: r.Name, Source: r.Source, CreatedAt: r.CreatedAt}
}

func (o *Orchestrator) toDeploy(row db.DeployRow) Deploy {
	d := Deploy{
		ID:           row.ID,
		Repo:         row.RepoName,
		SHA:          row.SHA,
		ShortSHA:     row.ShortSHA,
		Ref:          row.Ref,
		Branch:       row.Branch,
		AuthorName:   row.AuthorName,
		AuthorEmail:  row.AuthorEmail,
		FeHash:       row.FeHash,
		BeHash:       row.BeHash,
		Status:       row.Status,
		Error:        row.Error,
		AttemptCount: row.AttemptCount,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
	if row.Status == db.DeployReady {
		d.PreviewURL = o.previewURL(row)
		if row.BeHash != "" {
			d.Process = o.super.Status(supervise.BackendKey(row.RepoID, row.BeHash))
		}
		if row.FeHash != "" {
			if _, err := o.database.GetFrontendArtifact(row.RepoID, row.FeHash); err == nil {
				d.FeProcess = o.super.Status(supervise.FrontendKey(row.RepoID, row.FeHash, row.BeHash))
			}
		}
	}
	return d
}

func (o *Orchestrator) previewURL(row db.DeployRow) string {
	host := fmt.Sprintf("%s.%s.%s", row.ShortSHA, row.RepoName, o.opts.PreviewDomain)
	if _, port, err := net.SplitHostPort(o.opts.Addr); err == nil && port != "" && port != "80" {
		host += ":" + port
	}
	return "http://" + host + "/"
}
