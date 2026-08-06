// Package build turns deploy requests into published artifacts. A small
// worker pool drains a queue of deploy IDs; each build extracts the commit's
// tree once, reads preview.toml from that commit, computes the partition
// hashes, and builds only the sides whose artifacts don't exist yet.
// Concurrent deploys that share a hash are deduplicated with singleflight.
//
// Build logs are keyed by artifact hash, not deploy ID — that's what a build
// execution actually is, and deploys sharing a hash share its log. Deploy
// rows carry the paths.
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/hashkey"
	"github.com/jmelahman/local-preview/internal/manifest"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

// DefaultBuildTimeout bounds a single build step.
const DefaultBuildTimeout = 10 * time.Minute

// ManifestName is the default contract file read from the committed tree.
const ManifestName = "preview.toml"

// ManifestRef locates a preview manifest in a target repo: a TOML file at
// the repo root, optionally rooted at a top-level table (so embedders can
// host the manifest inside a larger config file).
type ManifestRef struct {
	Path  string
	Table string
}

func (r ManifestRef) String() string {
	if r.Table == "" {
		return r.Path
	}
	return fmt.Sprintf("%s [%s]", r.Path, r.Table)
}

// Queue coordinates deploys: request → queued row → worker → artifacts →
// state provisioning → ready.
type Queue struct {
	db      *db.Store
	git     *gitrepo.Manager
	files   *store.Store
	super   *supervise.Manager
	logsDir string
	runner  Runner

	manifestRefs     []ManifestRef
	localManifestDir string
	buildTimeout     time.Duration
	sf           singleflight.Group
	work         chan int64

	mu      sync.Mutex
	rebuild map[int64]bool
}

// NewQueue wires the pipeline. runner may be nil for the default HostRunner.
// Call Start to launch workers.
func NewQueue(database *db.Store, git *gitrepo.Manager, files *store.Store, super *supervise.Manager, logsDir string, runner Runner) *Queue {
	if runner == nil {
		// The default routes image-declared steps into containers and
		// everything else to the host.
		runner = &autoRunner{}
	}
	return &Queue{
		db:           database,
		git:          git,
		files:        files,
		super:        super,
		logsDir:      logsDir,
		runner:       runner,
		manifestRefs: []ManifestRef{{Path: ManifestName}},
		buildTimeout: DefaultBuildTimeout,
		work:         make(chan int64, 256),
		rebuild:      make(map[int64]bool),
	}
}

// SetManifestRefs replaces the manifest locations tried (in order) at each
// deployed commit. Call before Start.
func (q *Queue) SetManifestRefs(refs []ManifestRef) {
	if len(refs) > 0 {
		q.manifestRefs = refs
	}
}

// SetLocalManifestDir sets a directory searched for out-of-repo manifests
// (<dir>/<repo>.toml, plain preview.toml schema) when no in-repo source
// matches — onboarding a repo whose upstream can't carry a manifest. Call
// before Start.
func (q *Queue) SetLocalManifestDir(dir string) {
	q.localManifestDir = dir
}

// Start launches n build workers and re-enqueues deploys interrupted by a
// previous shutdown.
func (q *Queue) Start(ctx context.Context, n int) {
	if ids, err := q.db.ListUnfinishedDeployIDs(); err == nil {
		for _, id := range ids {
			q.enqueue(id)
		}
	}
	for range n {
		go q.worker(ctx)
	}
}

func (q *Queue) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-q.work:
			q.process(ctx, id)
		}
	}
}

func (q *Queue) enqueue(id int64) {
	q.work <- id
}

// RequestDeploy is the single entry point for every trigger adapter (CLI,
// hook, API, later poller/webhook): resolve ref → upsert deploy row →
// enqueue. Idempotent per (repo, sha): an in-flight or ready deploy is
// returned as-is unless rebuild is set.
func (q *Queue) RequestDeploy(ctx context.Context, repoName, ref string, rebuild bool) (db.DeployRow, error) {
	repo, err := q.db.GetRepoByName(repoName)
	if err != nil {
		return db.DeployRow{}, err
	}
	gr := q.git.Open(repo.Name)

	// Branch/tag names resolve against local refs, which go stale — fetch
	// first. Full or abbreviated shas use ResolveRef's fetch-on-miss retry.
	if !looksLikeSHA(ref) {
		if err := gr.Fetch(ctx); err != nil {
			return db.DeployRow{}, fmt.Errorf("fetch %s: %w", repoName, err)
		}
	}
	sha, err := gr.ResolveRef(ctx, ref)
	if err != nil {
		return db.DeployRow{}, err
	}
	branch := ""
	if !looksLikeSHA(ref) {
		branch = ref
	}

	d, err := q.db.GetDeployBySHA(repo.ID, sha)
	switch {
	case err == nil:
		switch {
		case d.Status == db.DeployQueued || d.Status == db.DeployBuilding:
			// Already in flight.
		case d.Status == db.DeployReady && !rebuild:
			// Nothing to do.
		default:
			// failed, evicted, or an explicit rebuild of a ready deploy.
			if err := q.db.ResetDeploy(d.ID); err != nil {
				return db.DeployRow{}, err
			}
			q.setRebuild(d.ID, rebuild)
			q.enqueue(d.ID)
		}
	case errors.Is(err, db.ErrNotFound):
		d, err = q.db.CreateDeploy(repo.ID, sha, branch)
		if err != nil {
			return db.DeployRow{}, err
		}
		q.setRebuild(d.ID, rebuild)
		q.enqueue(d.ID)
	default:
		return db.DeployRow{}, err
	}
	return q.db.GetDeployByID(d.ID)
}

func (q *Queue) setRebuild(id int64, v bool) {
	q.mu.Lock()
	if v {
		q.rebuild[id] = true
	} else {
		delete(q.rebuild, id)
	}
	q.mu.Unlock()
}

func (q *Queue) takeRebuild(id int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	v := q.rebuild[id]
	delete(q.rebuild, id)
	return v
}

func (q *Queue) process(ctx context.Context, id int64) {
	row, err := q.db.GetDeployByID(id)
	if err != nil {
		log.Printf("build: load deploy %d: %v", id, err)
		return
	}
	if row.Status == db.DeployReady || row.Status == db.DeployEvicted {
		return
	}
	if err := q.db.SetDeployBuilding(id); err != nil {
		log.Printf("build: mark building %d: %v", id, err)
		return
	}
	if err := q.buildDeploy(ctx, row, q.takeRebuild(id)); err != nil {
		log.Printf("build: deploy %d (%s@%s) failed: %v", id, row.RepoName, row.ShortSHA, err)
		q.db.SetDeployFailed(id, truncate(err.Error(), 500))
		return
	}
	q.db.SetDeployReady(id)
	log.Printf("build: deploy %d (%s@%s) ready", id, row.RepoName, row.ShortSHA)
}

func (q *Queue) buildDeploy(ctx context.Context, row db.DeployRow, rebuild bool) error {
	gr := q.git.Open(row.RepoName)

	m, err := q.loadManifest(ctx, gr, row)
	if err != nil {
		return err
	}
	entries, err := gr.LsTree(ctx, row.SHA, "")
	if err != nil {
		return err
	}
	feHash, err := hashkey.Frontend(m.Frontend, entries)
	if err != nil {
		return err
	}
	beHash, err := hashkey.Backend(m.Backend, m.Frontend.Path, entries)
	if err != nil {
		return err
	}
	feLog := q.logPath(row.RepoName, "fe", feHash)
	beLog := q.logPath(row.RepoName, "be", beHash)
	if err := q.db.SetDeployHashes(row.ID, feHash, beHash, feLog, beLog); err != nil {
		return err
	}

	needFe := rebuild || !q.files.HasFrontend(row.RepoName, feHash)
	needBe := rebuild || !q.files.HasBackend(row.RepoName, beHash)

	// One extraction serves both sides; skipped entirely on full cache hits.
	var scratch string
	if needFe || needBe {
		dir, cleanup, err := q.files.NewScratchDir("build")
		if err != nil {
			return err
		}
		defer cleanup()
		if err := gr.Archive(ctx, row.SHA, dir); err != nil {
			return err
		}
		scratch = dir
	}

	if needFe {
		key := row.RepoName + ":fe:" + feHash
		if _, err, _ := q.sf.Do(key, func() (any, error) {
			return nil, q.buildFrontend(ctx, row, m.Frontend, scratch, feHash, feLog, rebuild)
		}); err != nil {
			return fmt.Errorf("frontend build: %w (log: %s)", err, feLog)
		}
	}
	if needBe {
		key := row.RepoName + ":be:" + beHash
		if _, err, _ := q.sf.Do(key, func() (any, error) {
			return nil, q.buildBackend(ctx, row, m.Backend, scratch, beHash, beLog, rebuild)
		}); err != nil {
			return fmt.Errorf("backend build: %w (log: %s)", err, beLog)
		}
	}

	// Provision state before ready: "ready" must imply the state dir exists
	// so the proxy's hot path never provisions anything. Run configs carry
	// the top-level networks list alongside the section — it's run-time
	// context the supervisor needs but not part of the artifact hash.
	runCfg, err := json.Marshal(struct {
		manifest.Backend
		Networks []string `json:"networks,omitempty"`
	}{m.Backend, m.Networks})
	if err != nil {
		return err
	}
	repo, err := q.db.GetRepoByName(row.RepoName)
	if err != nil {
		return err
	}
	if len(m.Frontend.Run) > 0 {
		feCfg, err := json.Marshal(struct {
			manifest.Frontend
			Networks []string `json:"networks,omitempty"`
		}{m.Frontend, m.Networks})
		if err != nil {
			return err
		}
		if err := q.db.CreateFrontendArtifact(db.FrontendArtifact{
			RepoID: repo.ID, FeHash: feHash, RunConfig: string(feCfg),
		}); err != nil {
			return err
		}
	}
	return q.super.ForkOrInitStateDir(ctx, gr, repo.ID, row.RepoName, beHash, row.SHA, string(runCfg))
}

// loadManifest reads the first present manifest source at the deployed
// commit, then falls back to the local manifest dir (<dir>/<repo>.toml,
// read from the server's disk rather than the committed tree). A missing
// file or table means "try the next source"; a present but invalid manifest
// fails the deploy with its parse error.
func (q *Queue) loadManifest(ctx context.Context, gr gitrepo.Repo, row db.DeployRow) (manifest.Manifest, error) {
	tried := make([]string, 0, len(q.manifestRefs)+1)
	for _, ref := range q.manifestRefs {
		tried = append(tried, ref.String())
		raw, err := gr.ReadFile(ctx, row.SHA, ref.Path)
		if err != nil {
			continue
		}
		m, err := manifest.ParseAt(raw, ref.Table)
		if errors.Is(err, manifest.ErrNoManifest) {
			continue
		}
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("%s: %w", ref, err)
		}
		return m, nil
	}
	if q.localManifestDir != "" {
		local := filepath.Join(q.localManifestDir, row.RepoName+".toml")
		tried = append(tried, local)
		if raw, err := os.ReadFile(local); err == nil {
			m, err := manifest.Parse(raw)
			if err != nil {
				return manifest.Manifest{}, fmt.Errorf("%s: %w", local, err)
			}
			return m, nil
		}
	}
	return manifest.Manifest{}, fmt.Errorf(
		"no preview manifest at %s (looked for %s) — is the repo onboarded?",
		row.ShortSHA, strings.Join(tried, ", "))
}

func (q *Queue) buildFrontend(ctx context.Context, row db.DeployRow, fe manifest.Frontend, scratch, hash, logPath string, overwrite bool) error {
	logF, err := openLog(logPath)
	if err != nil {
		return err
	}
	defer logF.Close()
	for _, step := range fe.Build {
		if err := q.runStep(ctx, row, scratch, fe.Path, fe.Image, step, logF); err != nil {
			return err
		}
	}
	if len(fe.Run) > 0 {
		// Process-mode frontend: the whole built tree is the artifact — the
		// server runs from it the way backends do; there is no static dist.
		return q.files.PublishFrontend(row.RepoName, hash, filepath.Join(scratch, fe.Path), overwrite)
	}
	dist := filepath.Join(scratch, fe.Path, fe.Dist)
	if st, err := os.Stat(dist); err != nil || !st.IsDir() {
		return fmt.Errorf("frontend.dist %q was not produced by the build", fe.Dist)
	}
	return q.files.PublishFrontend(row.RepoName, hash, dist, overwrite)
}

func (q *Queue) buildBackend(ctx context.Context, row db.DeployRow, be manifest.Backend, scratch, hash, logPath string, overwrite bool) error {
	logF, err := openLog(logPath)
	if err != nil {
		return err
	}
	defer logF.Close()
	for _, step := range be.Build {
		if err := q.runStep(ctx, row, scratch, be.Path, be.Image, step, logF); err != nil {
			return err
		}
	}
	return q.files.PublishBackend(row.RepoName, hash, filepath.Join(scratch, be.Path), overwrite)
}

func (q *Queue) runStep(ctx context.Context, row db.DeployRow, scratch, dir, image string, argv []string, logF io.Writer) error {
	cctx, cancel := context.WithTimeout(ctx, q.buildTimeout)
	defer cancel()
	fmt.Fprintf(logF, "$ %s\n", strings.Join(argv, " "))
	return q.runner.Run(cctx, RunSpec{
		RepoName:   row.RepoName,
		SHA:        row.SHA,
		ScratchDir: scratch,
		Dir:        dir,
		Argv:       argv,
		Image:      image,
	}, logF)
}

func (q *Queue) logPath(repoName, kind, hash string) string {
	return filepath.Join(q.logsDir, repoName, kind, hash+".log")
}

// openLog truncates: a (re)build execution owns its per-hash log.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "# build started %s\n", time.Now().UTC().Format(time.RFC3339))
	return f, nil
}

// looksLikeSHA reports whether ref is plausibly an (abbreviated) commit sha
// rather than a branch or tag name.
func looksLikeSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 64 {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
