package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmelahman/local-preview/internal/api"
	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/clone"
	"github.com/jmelahman/local-preview/internal/config"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/githuboidc"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/proxy"
	"github.com/jmelahman/local-preview/internal/retain"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
	"github.com/jmelahman/local-preview/internal/watch"
)

// version is populated at build time via -ldflags -X (see Dockerfile /
// .goreleaser.yaml). Use `git describe --tags --always --dirty` so a single
// string carries tag, distance, short sha, and dirty marker. Falls back to
// runtime/debug VCS info for dev builds without ldflags.
var version = ""

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version string `json:"version"`
}

// Build returns the build metadata for the running binary, falling back to
// runtime/debug VCS info when ldflags weren't set.
func Build() BuildInfo {
	if version != "" {
		return BuildInfo{Version: version}
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev string
		var modified bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if rev != "" {
			short := rev
			if len(short) > 7 {
				short = short[:7]
			}
			if modified {
				short += "-dirty"
			}
			return BuildInfo{Version: short}
		}
	}
	return BuildInfo{Version: "dev"}
}

// serveOptions carries `preview serve`'s flag values into run.
type serveOptions struct {
	addr             string
	dataDir          string
	inMemory         bool
	previewDomain    string
	previewBaseURL   string
	buildConcurrency int
	maxWarm          int
	pollInterval     time.Duration
	githubSecret     string
	githubOIDCAud    string
	githubOIDCIssuer string
}

func Root() *cobra.Command {
	var opts serveOptions

	cmd := &cobra.Command{
		Use:     "preview",
		Short:   "Local preview-deployment orchestrator",
		Version: Build().Version,
	}

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(opts)
		},
	}
	serve.Flags().StringVar(&opts.addr, "addr", ":8080", "HTTP listen address")
	serve.Flags().StringVar(&opts.dataDir, "data-dir", "", "Override data directory (default: $PREVIEW_DATA_DIR or XDG)")
	serve.Flags().BoolVar(&opts.inMemory, "in-memory", false, "Use an ephemeral in-memory SQLite database; all data is discarded on shutdown")
	serve.Flags().StringVar(&opts.previewDomain, "preview-domain", "", "Base domain previews are served under (default: $PREVIEW_DOMAIN or preview.localhost)")
	serve.Flags().StringVar(&opts.previewBaseURL, "preview-base-url", "", "Public base URL of previews, e.g. https://preview.example.com — sets the scheme, domain, and port of generated preview URLs when the server sits behind a proxy (default: $PREVIEW_BASE_URL)")
	serve.Flags().IntVar(&opts.buildConcurrency, "build-concurrency", 2, "Number of deploys built in parallel")
	serve.Flags().IntVar(&opts.maxWarm, "max-warm", 8, "Maximum concurrently running preview processes; the least-recently-used are stopped beyond it (0 = unlimited)")
	serve.Flags().DurationVar(&opts.pollInterval, "poll-interval", watch.DefaultInterval, "How often watched repos are fetched for new commits (0 disables watching)")
	serve.Flags().StringVar(&opts.githubSecret, "github-webhook-secret", "", "Shared secret validating GitHub webhook deliveries (default: $PREVIEW_GITHUB_WEBHOOK_SECRET; empty disables the endpoint)")
	serve.Flags().StringVar(&opts.githubOIDCAud, "github-oidc-audience", "", "Expected audience of GitHub Actions OIDC tokens (default: $PREVIEW_GITHUB_OIDC_AUDIENCE); setting it requires uploads to authenticate with a valid token bound to the target repo")
	serve.Flags().StringVar(&opts.githubOIDCIssuer, "github-oidc-issuer", githuboidc.DefaultIssuer, "GitHub Actions OIDC issuer (default: $PREVIEW_GITHUB_OIDC_ISSUER or the hosted issuer; override for GitHub Enterprise Server)")
	cmd.AddCommand(serve)

	addClientCommands(cmd)

	return cmd
}

func run(opts serveOptions) error {
	if opts.githubSecret == "" {
		opts.githubSecret = os.Getenv("PREVIEW_GITHUB_WEBHOOK_SECRET")
	}
	if opts.githubOIDCAud == "" {
		opts.githubOIDCAud = os.Getenv("PREVIEW_GITHUB_OIDC_AUDIENCE")
	}
	if iss := os.Getenv("PREVIEW_GITHUB_OIDC_ISSUER"); iss != "" && opts.githubOIDCIssuer == githuboidc.DefaultIssuer {
		opts.githubOIDCIssuer = iss
	}
	// Configuring an audience turns on OIDC auth for the upload endpoints; a
	// nil verifier leaves them unauthenticated, as before.
	var uploadAuth api.UploadVerifier
	if opts.githubOIDCAud != "" {
		uploadAuth = githuboidc.NewVerifier(opts.githubOIDCIssuer, opts.githubOIDCAud)
		log.Printf("upload endpoints require GitHub Actions OIDC (issuer %s, audience %s)",
			opts.githubOIDCIssuer, opts.githubOIDCAud)
	}
	cfg, err := config.Load(config.Options{
		DataDir:        opts.dataDir,
		PreviewDomain:  opts.previewDomain,
		PreviewBaseURL: opts.previewBaseURL,
		Addr:           opts.addr,
	})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()

	dbPath := cfg.DBPath()
	if opts.inMemory {
		log.Printf("WARNING: --in-memory set; using ephemeral SQLite, all data is lost on shutdown")
		dbPath = ":memory:"
	}
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	files := store.New(cfg.ArtifactsDir(), cfg.StateDir(), cfg.TmpDir())
	if err := files.SweepTmp(24 * time.Hour); err != nil {
		log.Printf("sweep tmp: %v", err)
	}
	gitMgr := gitrepo.NewManager(cfg.ReposDir())
	super := supervise.New(database, files, cfg.LogsDir())
	super.ReclaimOrphans()
	super.SetMaxWarm(opts.maxWarm)
	super.StartReaper(workCtx)
	queue := build.NewQueue(database, gitMgr, files, super, cfg.LogsDir(), nil)
	queue.SetManifestRefs([]build.ManifestRef{
		{Path: build.ManifestName},
		{Path: ".kanban.toml", Table: "previews"},
	})
	if dir := config.ManifestsDir(); dir != "" {
		queue.SetLocalManifestDir(dir)
	}
	queue.Start(workCtx, opts.buildConcurrency)
	watcher := watch.New(database, gitMgr, queue, opts.pollInterval)
	watcher.Start(workCtx)
	cloner := clone.New(database, gitMgr, watcher.Kick)
	cloner.Start(workCtx)
	sweeper := retain.New(database, super, files)
	sweeper.Start(workCtx)

	apex := api.NewMux(api.Deps{
		Store:               database,
		Build:               api.BuildInfo(Build()),
		Config:              cfg,
		Git:                 gitMgr,
		Queue:               queue,
		Super:               super,
		Cloner:              cloner,
		Watcher:             watcher,
		Files:               files,
		Sweeper:             sweeper,
		DBPath:              dbPath,
		GitHubWebhookSecret: opts.githubSecret,
		UploadAuth:          uploadAuth,
	})
	router := proxy.New(database, files, super, cfg.Preview.Domain, apex)

	srv := &http.Server{
		Addr:              opts.addr,
		Handler:           recoverPanics(logRequests(compressResponses(router))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s (previews at %s)", opts.addr, cfg.Preview.URL("<sha>", "<repo>"))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(ctx)
	stopWork()
	super.StopAll()
	return err
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// recoverPanics is the outermost middleware. It catches panics from any
// downstream handler so the process survives, logs the trace, and writes a
// 500 response.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			log.Printf("panic recovered: %v\n%s", rec, debug.Stack())
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}
