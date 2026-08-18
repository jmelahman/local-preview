package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmelahman/local-preview/internal/api"
	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/clone"
	"github.com/jmelahman/local-preview/internal/config"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/fleet"
	"github.com/jmelahman/local-preview/internal/githuboidc"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/proxy"
	"github.com/jmelahman/local-preview/internal/reconcile"
	"github.com/jmelahman/local-preview/internal/retain"
	"github.com/jmelahman/local-preview/internal/s3store"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
	"github.com/jmelahman/local-preview/internal/watch"
	"github.com/jmelahman/local-preview/internal/workerapi"
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
	ssoClientID      string
	ssoClientSecret  string
	ssoCallbackURL   string
	ssoAllowedOrg    string
	ssoAllowedTeam   string
	ssoAllowedLogins string
	ssoAllowedEmails string
	s3Endpoint       string
	s3Bucket         string
	s3Prefix         string
	s3Region         string
	s3AccessKey      string
	s3SecretKey      string
	s3UseSSL         bool

	cacheMaxArtifactBytes int64

	role            string
	workerSecret    string
	workerListen    string
	workerEndpoint  string
	workerEndpoints string
	workerHost      string
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
	serve.Flags().StringVar(&opts.ssoClientID, "sso-github-client-id", "", "GitHub OAuth App client ID enabling interactive login (default: $PREVIEW_SSO_GITHUB_CLIENT_ID); setting it requires a sign-in for the dashboard, API, and previews")
	serve.Flags().StringVar(&opts.ssoClientSecret, "sso-github-client-secret", "", "GitHub OAuth App client secret (default: $PREVIEW_SSO_GITHUB_CLIENT_SECRET)")
	serve.Flags().StringVar(&opts.ssoCallbackURL, "sso-callback-url", "", "Public OAuth callback URL, e.g. https://preview.example.com/api/auth/callback — must match the GitHub OAuth App exactly (default: $PREVIEW_SSO_CALLBACK_URL)")
	serve.Flags().StringVar(&opts.ssoAllowedOrg, "sso-allowed-org", "", "Allow members of this GitHub org to sign in (default: $PREVIEW_SSO_ALLOWED_ORG)")
	serve.Flags().StringVar(&opts.ssoAllowedTeam, "sso-allowed-team", "", "Narrow --sso-allowed-org to this team slug (default: $PREVIEW_SSO_ALLOWED_TEAM)")
	serve.Flags().StringVar(&opts.ssoAllowedLogins, "sso-allowed-logins", "", "Comma-separated GitHub usernames allowed to sign in (default: $PREVIEW_SSO_ALLOWED_LOGINS)")
	serve.Flags().StringVar(&opts.ssoAllowedEmails, "sso-allowed-emails", "", "Comma-separated verified emails allowed to sign in (default: $PREVIEW_SSO_ALLOWED_EMAILS)")
	serve.Flags().StringVar(&opts.s3Endpoint, "s3-endpoint", "", "S3 (or MinIO) endpoint host:port enabling the durable artifact tier — built artifacts are uploaded and hydrated instead of rebuilt after eviction (default: $PREVIEW_S3_ENDPOINT; empty disables it)")
	serve.Flags().StringVar(&opts.s3Bucket, "s3-bucket", "", "Bucket for the durable artifact tier (default: $PREVIEW_S3_BUCKET; required to enable it)")
	serve.Flags().StringVar(&opts.s3Prefix, "s3-prefix", "", "Optional key prefix within the artifact-tier bucket (default: $PREVIEW_S3_PREFIX)")
	serve.Flags().StringVar(&opts.s3Region, "s3-region", "", "Region for the artifact-tier bucket (default: $PREVIEW_S3_REGION)")
	serve.Flags().StringVar(&opts.s3AccessKey, "s3-access-key", "", "Access key for the artifact tier (default: $PREVIEW_S3_ACCESS_KEY or $AWS_ACCESS_KEY_ID)")
	serve.Flags().StringVar(&opts.s3SecretKey, "s3-secret-key", "", "Secret key for the artifact tier (default: $PREVIEW_S3_SECRET_KEY or $AWS_SECRET_ACCESS_KEY)")
	serve.Flags().BoolVar(&opts.s3UseSSL, "s3-use-ssl", true, "Use TLS for the artifact-tier endpoint (set false for local MinIO over http)")
	serve.Flags().Int64Var(&opts.cacheMaxArtifactBytes, "cache-max-artifact-bytes", 0, "Soft cap on resident (local-disk) artifact bytes; the coldest artifacts are swept back to the durable tier above it. Requires the artifact tier; 0 disables cache eviction and keeps every artifact resident (default: $PREVIEW_CACHE_MAX_ARTIFACT_BYTES)")
	serve.Flags().StringVar(&opts.role, "role", "all", "Serving role: 'all' (single node — API, dashboard, proxy, and local process supervision), 'control' (route previews to a worker tier), or 'worker' (supervise processes on behalf of a control node)")
	serve.Flags().StringVar(&opts.workerSecret, "worker-secret", "", "Shared secret authenticating the internal worker API in both directions (default: $PREVIEW_WORKER_SECRET)")
	serve.Flags().StringVar(&opts.workerListen, "worker-listen", "", "Private address to expose the internal worker API on, e.g. :9100 — MUST NOT be internet/ALB-reachable; empty disables it (roles 'worker'/'all')")
	serve.Flags().StringVar(&opts.workerEndpoint, "worker-endpoint", "", "Control node only: a worker's private worker-API base URL, e.g. http://10.0.1.5:9100 (default: $PREVIEW_WORKER_ENDPOINT)")
	serve.Flags().StringVar(&opts.workerEndpoints, "worker-endpoints", "", "Control node only: comma-separated worker-API base URLs forming the fleet, e.g. http://10.0.1.5:9100,http://10.0.1.6:9100 (default: $PREVIEW_WORKER_ENDPOINTS). Combined with --worker-endpoint")
	serve.Flags().StringVar(&opts.workerHost, "worker-host", "", "Control node only: the routable host the worker's preview processes are reached on, e.g. 10.0.1.5 (default: the --worker-endpoint host)")
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
	envDefault(&opts.ssoClientID, "PREVIEW_SSO_GITHUB_CLIENT_ID")
	envDefault(&opts.ssoClientSecret, "PREVIEW_SSO_GITHUB_CLIENT_SECRET")
	envDefault(&opts.ssoCallbackURL, "PREVIEW_SSO_CALLBACK_URL")
	envDefault(&opts.ssoAllowedOrg, "PREVIEW_SSO_ALLOWED_ORG")
	envDefault(&opts.ssoAllowedTeam, "PREVIEW_SSO_ALLOWED_TEAM")
	envDefault(&opts.ssoAllowedLogins, "PREVIEW_SSO_ALLOWED_LOGINS")
	envDefault(&opts.ssoAllowedEmails, "PREVIEW_SSO_ALLOWED_EMAILS")
	envDefault(&opts.s3Endpoint, "PREVIEW_S3_ENDPOINT")
	envDefault(&opts.s3Bucket, "PREVIEW_S3_BUCKET")
	envDefault(&opts.s3Prefix, "PREVIEW_S3_PREFIX")
	envDefault(&opts.s3Region, "PREVIEW_S3_REGION")
	envDefault(&opts.s3AccessKey, "PREVIEW_S3_ACCESS_KEY")
	envDefault(&opts.s3AccessKey, "AWS_ACCESS_KEY_ID")
	envDefault(&opts.s3SecretKey, "PREVIEW_S3_SECRET_KEY")
	envDefault(&opts.s3SecretKey, "AWS_SECRET_ACCESS_KEY")
	envDefaultInt64(&opts.cacheMaxArtifactBytes, "PREVIEW_CACHE_MAX_ARTIFACT_BYTES")
	envDefault(&opts.workerSecret, "PREVIEW_WORKER_SECRET")
	envDefault(&opts.workerEndpoint, "PREVIEW_WORKER_ENDPOINT")
	envDefault(&opts.workerEndpoints, "PREVIEW_WORKER_ENDPOINTS")
	switch opts.role {
	case "all", "control", "worker":
	default:
		return fmt.Errorf("--role must be one of all|control|worker, got %q", opts.role)
	}
	if opts.role == "control" && len(workerEndpoints(opts)) > 0 && opts.workerSecret == "" {
		return fmt.Errorf("--role=control with worker endpoints requires --worker-secret")
	}
	if opts.workerListen != "" && opts.workerSecret == "" {
		return fmt.Errorf("--worker-listen requires --worker-secret (the worker API is a remote-code-execution surface)")
	}
	// Setting the client ID turns on interactive SSO for the dashboard, API,
	// and previews; leaving it unset keeps the historical open behavior.
	var sso api.SSOProvider
	var dashboardOrigin string
	var cookiesSecure bool
	if opts.ssoClientID != "" {
		provider, origin, secure, err := buildSSO(opts)
		if err != nil {
			return err
		}
		sso, dashboardOrigin, cookiesSecure = provider, origin, secure
		log.Printf("SSO login enabled (dashboard %s); dashboard, API, and previews require a GitHub sign-in", origin)
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
	// A configured bucket enables the durable artifact tier: built artifacts are
	// uploaded and, after eviction, hydrated instead of rebuilt. Fail closed so a
	// misconfigured bucket surfaces at startup rather than silently dropping every
	// upload. Must be set before Start, which launches the persist workers.
	var artifactTier *s3store.Tier
	if opts.s3Bucket != "" && opts.s3Endpoint != "" {
		t, err := s3store.New(s3store.Config{
			Endpoint:  opts.s3Endpoint,
			Bucket:    opts.s3Bucket,
			Prefix:    opts.s3Prefix,
			Region:    opts.s3Region,
			AccessKey: opts.s3AccessKey,
			SecretKey: opts.s3SecretKey,
			UseSSL:    opts.s3UseSSL,
			TmpDir:    cfg.TmpDir(),
		})
		if err != nil {
			return err
		}
		artifactTier = t
		queue.SetArtifactTier(t)
		log.Printf("artifact tier: s3 %s/%s (built artifacts persist and hydrate instead of rebuilding)",
			opts.s3Endpoint, opts.s3Bucket)
	}
	queue.Start(workCtx, opts.buildConcurrency)
	watcher := watch.New(database, gitMgr, queue, opts.pollInterval)
	watcher.Start(workCtx)
	cloner := clone.New(database, gitMgr, watcher.Kick)
	cloner.Start(workCtx)
	sweeper := retain.New(database, super, files)
	sweeper.Start(workCtx)
	// With a durable tier and a resident-cache cap set, sweep the coldest
	// artifacts off local disk periodically; a swept artifact re-hydrates on
	// next serve. Off by default (cap 0), so single-node instances keep every
	// artifact resident exactly as before.
	if opts.cacheMaxArtifactBytes > 0 && files.ArtifactTier() != nil {
		startCacheSweeper(workCtx, files, super, opts.cacheMaxArtifactBytes)
		log.Printf("artifact cache: resident cap %d bytes (coldest swept to durable tier, re-hydrated on serve)",
			opts.cacheMaxArtifactBytes)
	}
	// With a durable tier, reconcile guarantees every live deploy's artifacts
	// exist and verify in the bucket — closing gaps from pre-tier builds,
	// cache-hit builds that skipped persist, and crash-dropped uploads. It runs
	// once at startup and periodically thereafter.
	if artifactTier != nil {
		startReconciler(workCtx, reconcile.New(database, files, artifactTier))
	}

	deps := api.Deps{
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
		SSO:                 sso,
		DashboardOrigin:     dashboardOrigin,
		CookiesSecure:       cookiesSecure,
	}
	apex := api.AuthMiddleware(deps, api.NewMux(deps))

	// Serving transport: the proxy is address-based and transport-agnostic. By
	// default (and for a worker serving its own processes) it drives the local
	// Manager over loopback; a control node with a worker endpoint drives the
	// remote worker over the internal API instead. Both satisfy proxy.Backends.
	var backends proxy.Backends = supervise.LocalBackends{M: super}
	if opts.role == "control" {
		if endpoints := workerEndpoints(opts); len(endpoints) > 0 {
			reg := fleet.New(fleetStaleAfter)
			for _, ep := range endpoints {
				host := opts.workerHost
				if host == "" || len(endpoints) > 1 {
					host = hostOf(ep)
				}
				reg.Add(ep, workerapi.NewClient(ep, host, opts.workerSecret, nil))
				log.Printf("role=control: registered worker %s (processes at %s)", ep, host)
			}
			reg.StartHeartbeats(workCtx, fleetHeartbeatInterval)
			startFleetSignal(workCtx, reg)
			backends = reg
		} else {
			log.Printf("role=control with no --worker-endpoint(s): previews will serve locally")
		}
	}
	router := proxy.New(database, files, backends, cfg.Preview.Domain, apex)
	if sso != nil {
		router.SetPreviewAuth(true, dashboardOrigin, cookiesSecure)
		startSessionGC(workCtx, database)
	}

	// Expose the internal worker API when configured (worker/all roles). It is a
	// remote-code-execution surface — private listener, shared-secret only,
	// never ALB-reachable.
	var workerSrv *http.Server
	if opts.workerListen != "" && (opts.role == "worker" || opts.role == "all") {
		workerSrv = &http.Server{
			Addr:              opts.workerListen,
			Handler:           workerapi.NewServer(super, opts.workerSecret).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Printf("worker API listening on %s (private; shared-secret auth)", opts.workerListen)
			if err := workerSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("worker API listen: %v", err)
			}
		}()
	}

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
	if workerSrv != nil {
		workerSrv.Shutdown(ctx) //nolint:errcheck
	}
	stopWork()
	// Drain in-flight artifact uploads before stopping processes: the build
	// workers have stopped enqueuing (their context is cancelled), so this only
	// waits on uploads already started.
	queue.Stop()
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

// cacheSweepInterval is how often the resident-artifact cache is trimmed to its
// cap; cacheSweepMinAge shields an artifact published within it from eviction,
// so a just-built side whose asynchronous persist is still in flight is never
// swept before it lands durably.
const (
	cacheSweepInterval = time.Minute
	cacheSweepMinAge   = 10 * time.Minute
)

// startCacheSweeper periodically trims resident artifacts to maxBytes, coldest
// first, protecting any side with a live process. Returns immediately; the loop
// stops when ctx is cancelled.
func startCacheSweeper(ctx context.Context, files *store.Store, super *supervise.Manager, maxBytes int64) {
	go func() {
		t := time.NewTicker(cacheSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				live := super.LiveArtifacts()
				protect := func(repo, side, hash string) bool {
					_, ok := live[repo+"\x00"+side+"\x00"+hash]
					return ok
				}
				if freed, err := files.EvictCacheToWatermark(maxBytes, cacheSweepMinAge, protect); err != nil {
					log.Printf("artifact cache sweep: %v", err)
				} else if freed > 0 {
					log.Printf("artifact cache sweep: reclaimed %d bytes", freed)
				}
			}
		}
	}()
}

// reconcileInterval is how often the durable tier is reconciled against the
// live deploy set after the initial startup pass.
const reconcileInterval = time.Hour

// startReconciler runs an initial reconcile pass, then repeats it on an
// interval. Returns immediately; the loop stops when ctx is cancelled. A gap
// (an artifact missing from the tier and not resident locally) is logged
// loudly — it is unrecoverable without a rebuild and blocks a serve-only node.
func startReconciler(ctx context.Context, r *reconcile.Reconciler) {
	run := func() {
		rep, err := r.Reconcile(ctx)
		if err != nil {
			log.Printf("reconcile: %v", err)
			return
		}
		log.Printf("reconcile: %s", rep)
		if rep.Gaps > 0 {
			log.Printf("reconcile: WARNING %d artifact(s) missing from the tier and not resident locally: %v",
				rep.Gaps, rep.GapKeys)
		}
	}
	go func() {
		run()
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	}()
}

// hostOf extracts the host (without port) from a base URL, the default routable
// host for a worker's preview processes when --worker-host isn't given.
func hostOf(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return baseURL
}

// Fleet timings: how often the control node polls workers for capacity, how
// long a silent worker survives before placement treats it as gone, and how
// often the fleet-wide load signal is logged (the input a scale-out policy
// target-tracks).
const (
	fleetHeartbeatInterval = 10 * time.Second
	fleetStaleAfter        = 30 * time.Second
	fleetSignalInterval    = time.Minute
)

// workerEndpoints returns the deduplicated worker-API base URLs for the control
// node, merging the single --worker-endpoint and the comma-separated
// --worker-endpoints.
func workerEndpoints(opts serveOptions) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(opts.workerEndpoint)
	for _, e := range strings.Split(opts.workerEndpoints, ",") {
		add(e)
	}
	return out
}

// startFleetSignal periodically logs the fleet-wide load ratio (committed warm
// slots ÷ capacity) — the signal a scale-out policy (e.g. a CloudWatch
// target-tracking alarm scraping this line, or a sidecar publishing it as a
// custom metric) drives autoscaling from. Returns immediately.
func startFleetSignal(ctx context.Context, reg *fleet.Registry) {
	go func() {
		t := time.NewTicker(fleetSignalInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				running, capacity := reg.Capacity()
				log.Printf("fleet: load=%.2f (%d/%d warm slots)", reg.LoadRatio(), running, capacity)
			}
		}
	}()
}
